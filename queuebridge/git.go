package queuebridge

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// github is the repository today's gate queues trees on.
const github = "system-inc/adamic"

// A GitError is git exiting with a code its caller didn't allow, or timing out: what it printed says nothing, so
// nothing is decided from it this pass (#6gj7n9p: a fetch that failed whole left origin/main old, and its head went to
// the queue fresh).
type GitError struct {
	Text string
}

func (err *GitError) Error() string {
	return err.Text
}

// Clone is today's gate through this machine's clone of adamic, whose origin is GitHub: git's facts, the records on
// origin, gh for the branch that queues a tree, requeue.sh, and push-main.sh in check-only mode.
type Clone struct {
	Repository string
	PushMain   string
	RequeueSh  string
}

// run runs a command with a time limit: its stdout, stderr and exit code, and an error when it couldn't run or ran out
// of time.
func run(limit time.Duration, directory string, environment []string, stdin []byte, name string, arguments ...string) (string, string, int, error) {
	runContext, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	command := exec.CommandContext(runContext, name, arguments...)
	command.Dir = directory
	if environment != nil {
		command.Env = append(os.Environ(), environment...)
	}
	if stdin != nil {
		command.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	if runContext.Err() != nil {
		return stdout.String(), stderr.String(), -1, fmt.Errorf("timed out after %s", limit)
	}
	var exited *exec.ExitError
	if errors.As(err, &exited) {
		return stdout.String(), stderr.String(), exited.ExitCode(), nil
	}
	if err != nil {
		return stdout.String(), stderr.String(), -1, err
	}
	return stdout.String(), stderr.String(), 0, nil
}

// gitRun is git run in the clone when it exits with an allowed code (0 when none is named): its stdout, stderr and
// code, and a GitError for any other code or a timeout.
func (clone Clone) gitRun(arguments []string, allowed ...int) (string, string, int, error) {
	if len(allowed) == 0 {
		allowed = []int{0}
	}
	stdout, stderr, code, err := run(5*time.Minute, "", nil, nil, "git", append([]string{"-C", clone.Repository}, arguments...)...)
	what := strings.Join(arguments[:min(2, len(arguments))], " ")
	if err != nil {
		return "", "", code, &GitError{fmt.Sprintf("git %s: %v", what, err)}
	}
	if !slices.Contains(allowed, code) {
		said := strings.TrimSpace(stderr)
		return "", "", code, &GitError{fmt.Sprintf("git %s exited %d: %s", what, code, said[max(0, len(said)-300):])}
	}
	return stdout, stderr, code, nil
}

// git is git's stdout, trimmed, when it exits with an allowed code; a GitError otherwise.
func (clone Clone) git(arguments []string, allowed ...int) (string, error) {
	stdout, _, _, err := clone.gitRun(arguments, allowed...)
	return strings.TrimSpace(stdout), err
}

// holds is whether the clone holds sha as a commit, asked without fetching.
func (clone Clone) holds(sha string) bool {
	_, _, code, err := run(time.Minute, "", nil, nil, "git", "-C", clone.Repository, "cat-file", "-e", sha+"^{commit}")
	return err == nil && code == 0
}

// fetchSha fetches sha from origin: true once the clone holds it, false when origin says it has no such object, and a
// GitError for any other failure, since a network blip isn't an answer about the sha.
func (clone Clone) fetchSha(sha string) (bool, error) {
	_, stderr, code, err := clone.gitRun([]string{"fetch", "-q", "--no-tags", "origin", sha}, 0, 128)
	if err != nil {
		return false, err
	}
	if code == 0 {
		return true, nil
	}
	if strings.Contains(stderr, "not our ref") {
		return false, nil
	}
	said := strings.TrimSpace(stderr)
	return false, &GitError{fmt.Sprintf("git fetch %s exited 128: %s", sha[:min(12, len(sha))], said[max(0, len(said)-300):])}
}

// isAncestor is whether older is newer or an ancestor of it: merge-base answers 0 or 1, and anything else is a GitError.
func (clone Clone) isAncestor(older, newer string) (bool, error) {
	_, _, code, err := clone.gitRun([]string{"merge-base", "--is-ancestor", older, newer}, 0, 1)
	return code == 0 && err == nil, err
}

var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// mainHead is main's head as origin holds it now, asked of origin itself: no local ref that a failed fetch left behind.
func (clone Clone) mainHead() (string, error) {
	listed, err := clone.git([]string{"ls-remote", "origin", "refs/heads/main"})
	if err != nil {
		return "", err
	}
	fields := strings.Fields(listed)
	if len(fields) != 2 || !shaPattern.MatchString(fields[0]) || fields[1] != "refs/heads/main" {
		return "", &GitError{fmt.Sprintf("ls-remote named no main: %q", first(strings.Join(fields, " "), 200))}
	}
	return fields[0], nil
}

// pathLines are git's output lines, sorted, without empty ones or repeats.
func pathLines(output string) []string {
	paths := []string{}
	for _, path := range strings.Split(output, "\n") {
		if path != "" && !slices.Contains(paths, path) {
			paths = append(paths, path)
		}
	}
	slices.Sort(paths)
	return paths
}

// historyOf is every path a non-merge commit in base..sha touches: the queue refuses a change whose history reaches past
// its diff.
func (clone Clone) historyOf(base, sha string) ([]string, error) {
	listed, err := clone.git([]string{"log", "--no-merges", "--format=", "--name-only", base + ".." + sha})
	return pathLines(listed), err
}

// pythonTestPattern is push-main's Python test names: a test a non-test file names is gate logic (l.787-795).
var pythonTestPattern = regexp.MustCompile(`(_test\.py$|-test\.py$|(^|/)test_[^/]*\.py$)`)

// gateNamedOf is each Python test in the diff that a file other than a test, a .md or a .txt names in sha's tree, with
// those files.
func (clone Clone) gateNamedOf(sha string, diffPaths []string) ([]map[string]any, error) {
	named := []map[string]any{}
	for _, path := range diffPaths {
		if !pythonTestPattern.MatchString(path) {
			continue
		}
		// grep exits 1 when nothing names the test.
		found, err := clone.git([]string{"grep", "-l", "-F", "-e", filepath.Base(path), sha, "--", "."}, 0, 1)
		if err != nil {
			return nil, err
		}
		users := []string{}
		for _, line := range strings.Split(found, "\n") {
			_, user, ok := strings.Cut(line, ":")
			if ok && user != path && !testOnlyPattern.MatchString(user) && !strings.HasSuffix(user, ".md") && !strings.HasSuffix(user, ".txt") {
				users = append(users, user)
			}
		}
		if len(users) > 0 {
			slices.Sort(users)
			named = append(named, map[string]any{"path": path, "users": users})
		}
	}
	return named, nil
}

// patchId is the stable patch id of older..newer, or "" when there's no diff.
func (clone Clone) patchId(older, newer string) string {
	diff, _, _, err := run(5*time.Minute, "", nil, nil, "git", "-C", clone.Repository, "diff", older, newer)
	if err != nil || diff == "" {
		return ""
	}
	out, _, _, err := run(5*time.Minute, "", nil, []byte(diff), "git", "-C", clone.Repository, "patch-id", "--stable")
	if fields := strings.Fields(out); err == nil && len(fields) > 0 {
		return fields[0]
	}
	return ""
}

// revertOf is the commit among main's 30 newest first-parent commits whose inverse is exactly base..sha, or nil.
func (clone Clone) revertOf(base, sha string) (any, error) {
	change := clone.patchId(base, sha)
	if change == "" {
		return nil, nil
	}
	listed, err := clone.git([]string{"rev-list", "--first-parent", "-n", "30", "origin/main"})
	if err != nil {
		return nil, err
	}
	for _, commit := range strings.Fields(listed) {
		// --verify -q exits 1 for a root commit, which has no parent.
		parent, err := clone.git([]string{"rev-parse", "--verify", "-q", commit + "^1"}, 0, 1)
		if err != nil {
			return nil, err
		}
		if parent != "" && clone.patchId(commit, parent) == change {
			return commit, nil
		}
	}
	return nil, nil
}

// Facts are what git says about a submitted change, read from origin through the clone. A GitError when git can't
// say: the change then stays unchecked for the next pass.
func (clone Clone) Facts(sha, base string) (map[string]any, error) {
	// main's head, asked of origin: a witness is of main's tip only when this is its sha (#6gj7n9p).
	head, err := clone.mainHead()
	if err != nil {
		return nil, err
	}
	// main on its own, checked, so origin/main is fresh for the ancestry below; then the sha, whose absence is an answer.
	if _, err := clone.git([]string{"fetch", "-q", "--no-tags", "origin", "main"}); err != nil {
		return nil, err
	}
	fetched, err := clone.fetchSha(sha)
	if err != nil {
		return nil, err
	}
	if !fetched || !clone.holds(sha) {
		return map[string]any{"shaExists": false, "baseIsAncestor": false, "baseOnMain": false, "diffPaths": []string{}, "mainHead": head}, nil
	}
	diff, err := clone.git([]string{"diff", "--no-renames", "--name-only", base, sha})
	if err != nil {
		return nil, err
	}
	diffPaths := pathLines(diff)
	baseIsAncestor, err := clone.isAncestor(base, sha)
	if err != nil {
		return nil, err
	}
	baseOnMain, err := clone.isAncestor(base, "origin/main")
	if err != nil {
		return nil, err
	}
	history, err := clone.historyOf(base, sha)
	if err != nil {
		return nil, err
	}
	gateNamed, err := clone.gateNamedOf(sha, diffPaths)
	if err != nil {
		return nil, err
	}
	reverts, err := clone.revertOf(base, sha)
	if err != nil {
		return nil, err
	}
	return map[string]any{"shaExists": true, "baseIsAncestor": baseIsAncestor, "baseOnMain": baseOnMain, "diffPaths": diffPaths,
		"historyPaths": history, "gateNamed": gateNamed, "revertOf": reverts, "mainHead": head}, nil
}

// Record is tree's newest finished fast record on origin, or nil while none has finished.
func (clone Clone) Record(tree string) (*Record, error) {
	listed, err := clone.git([]string{"ls-remote", "origin", fmt.Sprintf("refs/heads/gate-logs/%s/*", tree[:min(12, len(tree))])})
	if err != nil {
		return nil, err
	}
	var refs []string
	for _, line := range strings.Split(listed, "\n") {
		if _, ref, ok := strings.Cut(line, "\t"); ok && strings.HasSuffix(ref, "/fast") {
			refs = append(refs, strings.TrimPrefix(ref, "refs/heads/"))
		}
	}
	// gate-logs/<tree12>/<stamp>/fast, newest stamp first.
	stamp := func(ref string) string {
		if parts := strings.Split(ref, "/"); len(parts) > 2 {
			return parts[2]
		}
		return ""
	}
	sort.SliceStable(refs, func(left, right int) bool { return stamp(refs[left]) > stamp(refs[right]) })
	if len(refs) == 0 {
		return nil, nil
	}
	fetch := []string{"fetch", "-q", "--no-tags", "origin"}
	for _, ref := range refs {
		fetch = append(fetch, fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", ref, ref))
	}
	if _, err := clone.git(fetch); err != nil {
		return nil, err
	}
	for _, ref := range refs {
		// A record still running may not have written its status or fast.json yet: only the files it holds are read.
		held, err := clone.git([]string{"ls-tree", "--name-only", "origin/" + ref})
		if err != nil {
			return nil, err
		}
		files := strings.Split(held, "\n")
		status := ""
		if slices.Contains(files, "status.txt") {
			text, err := clone.git([]string{"show", "origin/" + ref + ":status.txt"})
			if err != nil {
				return nil, err
			}
			line, _, _ := strings.Cut(text, "\n")
			word, _, _ := strings.Cut(line, ":")
			status = strings.TrimSpace(word)
		}
		if status != "green" && status != "red" && status != "void" {
			continue
		}
		fast := map[string]any{}
		if slices.Contains(files, "fast.json") {
			text, err := clone.git([]string{"show", "origin/" + ref + ":fast.json"})
			if err != nil {
				return nil, err
			}
			if json.Unmarshal([]byte(text), &fast) != nil {
				fast = map[string]any{}
			}
		}
		gated, _ := fast["gated"].(string)
		if gated == "" {
			gated, _ = fast["sha"].(string)
		}
		return &Record{Ref: ref, Status: status, Gated: gated}, nil
	}
	return nil, nil
}

// Failing is a record's failing top-level tests, from its test.jsonl.gz and fast.json, and false when the record can't
// be read that way (failingTests).
func (clone Clone) Failing(ref string) ([]string, bool) {
	raw, _, code, err := run(5*time.Minute, "", nil, nil, "git", "-C", clone.Repository, "show", "origin/"+ref+":test.jsonl.gz")
	var events []string
	if err == nil && code == 0 && raw != "" {
		reader, err := gzip.NewReader(strings.NewReader(raw))
		if err != nil {
			return nil, false
		}
		text, err := io.ReadAll(reader)
		if err != nil {
			return nil, false
		}
		events = strings.Split(strings.TrimSuffix(string(text), "\n"), "\n")
	}
	text, err := clone.git([]string{"show", "origin/" + ref + ":fast.json"})
	if err != nil {
		return nil, false
	}
	fast := map[string]any{}
	if text != "" && json.Unmarshal([]byte(text), &fast) != nil {
		return nil, false
	}
	return failingTests(events, fast)
}

// Parents are sha's parents, from git (a gate merge lives under refs/gate-merges, so it is fetched by sha): none when
// origin lacks it.
func (clone Clone) Parents(sha string) ([]string, error) {
	if !clone.holds(sha) {
		fetched, err := clone.fetchSha(sha)
		if err != nil || !fetched {
			return nil, err
		}
	}
	listed, err := clone.git([]string{"rev-list", "--parents", "-n", "1", sha})
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(listed)
	if len(fields) == 0 {
		return nil, nil
	}
	return fields[1:], nil
}

// Queue puts tree in front of fast-gate-watch as a cloud/land-* tip. True when the branch is there.
func (clone Clone) Queue(tree string) bool {
	branch := "cloud/land-queue-" + tree[:min(8, len(tree))]
	stdout, stderr, code, err := run(time.Minute, "", nil, nil, "gh", "api", "-X", "POST", "repos/"+github+"/git/refs", "-f", "ref=refs/heads/"+branch, "-f", "sha="+tree)
	return err == nil && (code == 0 || strings.Contains(stdout+stderr, "Reference already exists"))
}

// Requeue serves sha's fast job again (requeue.sh). True when it started.
func (clone Clone) Requeue(sha string) bool {
	_, _, code, err := run(2*time.Minute, "", nil, nil, "bash", clone.RequeueSh, sha)
	return err == nil && code == 0
}

// Check is push-main.sh in check-only mode (PUSH_MAIN_CHECK_ONLY=1), run from the checkout that holds it: every check it
// runs, the landing commit built, nothing pushed. A push-main that couldn't run reads as exit -1, which is never a
// refusal, so the check runs again next pass.
func (clone Clone) Check(arguments []string, label string) (int, string, string) {
	checkout := filepath.Dir(filepath.Dir(filepath.Dir(clone.PushMain)))
	stdout, stderr, code, err := run(time.Hour, checkout, []string{"PUSH_MAIN_CHECK_ONLY=1"}, nil, "bash", append(append([]string{clone.PushMain}, arguments...), label)...)
	if err != nil {
		return -1, stdout, stderr + "\npush-main didn't run: " + err.Error()
	}
	return code, stdout, stderr
}
