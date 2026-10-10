package queuebridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// A GitError is git exiting with a code its caller didn't allow, or timing out: what it printed says nothing, so
// nothing is decided from it this pass (#6gj7n9p: a fetch that failed whole left origin/main old, and its head went to
// the queue fresh).
type GitError struct {
	Text string
}

func (err *GitError) Error() string {
	return err.Text
}

// Clone is git's facts through this machine's clone of adamic, whose origin is the public
// https://github.com/system-inc/adamic.git: the bridge is keyless (only the pusher holds a key), by construction rather
// than by the machine's settings or the clone's own configuration. Facts refuses an origin that breaks PinUrl's rule
// (another host, a user or password in the url; a local path, as the tests' is, is no key either), and every git runs
// in keyless() with keylessSettings on its command line. A bare clone does, since main is fetched by an explicit refspec
// into refs/remotes/origin/main.
type Clone struct {
	Repository string
	// mirror is where a pin's github.com url is fetched from, for the tests' stand-in GitHub; nil fetches the url.
	mirror func(string) string
}

// keyless is the whole environment every git of the bridge runs in, made from nothing rather than added to the
// process's, so no inherited GIT_CONFIG_COUNT, GIT_CONFIG_PARAMETERS or credential variable reaches git: no global
// or system settings (no insteadOf rewriting https to ssh, no credential helper), no ~/.netrc (HOME is /dev/null), no
// prompt and no ssh. PATH and TMPDIR are the process's, and LC_ALL=C keeps git's words the ones the bridge reads.
func keyless() []string {
	environment := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.DevNull, "XDG_CONFIG_HOME=" + os.DevNull, "GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=" + falsePath, "SSH_ASKPASS=" + falsePath, "GIT_SSH_COMMAND=false", "LC_ALL=C"}
	if temporary := os.Getenv("TMPDIR"); temporary != "" {
		environment = append(environment, "TMPDIR="+temporary)
	}
	return environment
}

// cloneSettingPattern is every setting the clone's own configuration may hold: what `git clone --bare` (or init and
// `remote add`, as the tests' clones are made) writes for the public repository, and nothing else. Any other (a
// url.<base>.insteadOf whose base holds a user and password, an http.<url>.extraHeader, a credential, an include, a
// proxy, an ssh or askpass command) could carry a key or send git elsewhere, and command-line settings don't reset a
// url-scoped one, so the clone is refused, named, before git reads anything with it.
var cloneSettingPattern = regexp.MustCompile(`^(core\.(repositoryformatversion|filemode|bare|ignorecase|precomposeunicode|logallrefupdates|symlinks)|remote\.origin\.(url|fetch)|extensions\.objectformat)$`)

// ownSettings refuses a clone whose own configuration holds a setting outside cloneSettingPattern.
func (clone Clone) ownSettings() error {
	listed, err := clone.git([]string{"config", "--local", "--name-only", "--list"})
	if err != nil {
		return err
	}
	for _, name := range strings.Split(listed, "\n") {
		if name != "" && !cloneSettingPattern.MatchString(strings.ToLower(name)) {
			return &GitError{fmt.Sprintf("the clone's own configuration holds %s, which a keyless clone of the public repository never does: remove it, or clone again", name)}
		}
	}
	return nil
}

// keylessSettings are set on every git's command line, over anything a repository's own configuration says, since no
// environment drops that: no credential helper, no extra header (an Authorization one is a key), and the server's
// certificate verified.
var keylessSettings = []string{"-c", "credential.helper=", "-c", "http.extraHeader=", "-c", "http.sslVerify=true"}

// falsePath is the false command where this machine has it (/bin/false on Linux, /usr/bin/false on a Mac): an askpass
// that answers nothing.
var falsePath = func() string {
	if found, err := exec.LookPath("false"); err == nil {
		return found
	}
	return "/bin/false"
}()

// gitArguments are git's arguments for a command in repository, after keylessSettings.
func gitArguments(repository string, arguments ...string) []string {
	return append(append(append([]string{}, keylessSettings...), "-C", repository), arguments...)
}

// run runs a command with a time limit, in environment when it isn't nil (the whole environment, nothing of the
// process's): its stdout, stderr and exit code, and an error when it couldn't run or ran out of time.
func run(limit time.Duration, directory string, environment []string, stdin []byte, name string, arguments ...string) (string, string, int, error) {
	runContext, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	command := exec.CommandContext(runContext, name, arguments...)
	command.Dir = directory
	if environment != nil {
		command.Env = environment
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
	stdout, stderr, code, err := run(5*time.Minute, "", keyless(), nil, "git", gitArguments(clone.Repository, arguments...)...)
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
	_, _, code, err := run(time.Minute, "", keyless(), nil, "git", gitArguments(clone.Repository, "cat-file", "-e", sha+"^{commit}")...)
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

// testOnlyPattern is push-main.sh's testOnlyPattern, the same list by ruling (Kirk, Oct 8): a file it matches is a
// test, and naming a Python test doesn't make it gate logic.
var testOnlyPattern = regexp.MustCompile(`(_test\.go$|_test\.py$|-test\.py$|(^|/)test_[^/]*\.py$|/testdata/|^review/|(^|/)shards\.json$|^stage3/fixtures/|^stage3/meter/|^README\.md$)`)

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
	diff, _, _, err := run(5*time.Minute, "", keyless(), nil, "git", gitArguments(clone.Repository, "diff", older, newer)...)
	if err != nil || diff == "" {
		return ""
	}
	out, _, _, err := run(5*time.Minute, "", keyless(), []byte(diff), "git", gitArguments(clone.Repository, "patch-id", "--stable")...)
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

// Facts are what git says about a submitted change, read from origin through the clone, with its submodule pins each
// proven keyless-fetchable or not (PinsOf). A GitError when git can't
// say: the change then stays unchecked for the next pass.
func (clone Clone) Facts(sha, base string) (map[string]any, error) {
	// Keyless by construction: an origin that would need a key (ssh, an alias like github-lander) is refused before
	// anything is fetched from it, and the change waits.
	origin, err := clone.git([]string{"remote", "get-url", "origin"})
	if err != nil {
		return nil, err
	}
	// Its own configuration holds nothing that could carry a key or send git elsewhere, read before any git that reaches
	// the network runs with it.
	if err := clone.ownSettings(); err != nil {
		return nil, err
	}
	// The origin holds to the pins' rule (PinUrl): github.com over https, as owner/name, nothing in the url that is a key.
	// A local path, as the tests' clones have, needs no key either.
	if address, refused := PinUrl(origin); (refused != "" || address != origin) && !filepath.IsAbs(origin) {
		return nil, &GitError{fmt.Sprintf("the clone's origin %q isn't a github.com repository over https with no key in it: the bridge reads the public repository keyless", origin)}
	}
	// main's head, asked of origin: a witness is of main's tip only when this is its sha (#6gj7n9p).
	head, err := clone.mainHead()
	if err != nil {
		return nil, err
	}
	// main on its own, checked, so origin/main is fresh for the ancestry below (by an explicit refspec, which a bare
	// clone's remote lacks); then the sha, whose absence is an answer.
	if _, err := clone.git([]string{"fetch", "-q", "--no-tags", "origin", "+refs/heads/main:refs/remotes/origin/main"}); err != nil {
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
	pins, err := clone.PinsOf(sha)
	if err != nil {
		return nil, err
	}
	return map[string]any{"shaExists": true, "baseIsAncestor": baseIsAncestor, "baseOnMain": baseOnMain, "diffPaths": diffPaths,
		"historyPaths": history, "gateNamed": gateNamed, "revertOf": reverts, "pins": pins, "mainHead": head}, nil
}
