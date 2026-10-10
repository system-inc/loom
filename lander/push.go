package lander

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Git runs git in a repository with an optional environment on top of the process's, and gives back its stdout and
// stderr. Each call has five minutes.
func Git(repository string, environment []string, arguments ...string) (string, string, error) {
	context, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(context, "git", append([]string{"-C", repository}, arguments...)...)
	if environment != nil {
		command.Env = append(os.Environ(), environment...)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	return stdout.String(), stderr.String(), err
}

// Hands is the target branch on GitHub through the lander's own bare clone, whose origin is
// git@github-lander:system-inc/adamic.git.
type Hands struct {
	Repository string
	Branch     string
}

// Ref is the branch's full name, refs/heads/<branch>.
func (hands Hands) Ref() string {
	return "refs/heads/" + hands.Branch
}

// Tip is the branch's sha on GitHub, or "" when git can't read it or the branch isn't there: the lander never creates
// a branch, so a missing one holds every order.
func (hands Hands) Tip() string {
	stdout, _, err := Git(hands.Repository, nil, "ls-remote", "origin", hands.Ref())
	fields := strings.Fields(stdout)
	if err != nil || len(fields) < 2 || fields[1] != hands.Ref() || !shaPattern.MatchString(fields[0]) {
		return ""
	}
	return fields[0]
}

// A Kind is what became of one push.
type Kind string

const (
	// Landed: the branch is now exactly the sha.
	Landed Kind = "Landed"
	// Moved: the branch moved past the sha's base (the sha doesn't contain its tip, or it moved while the push was on
	// its way), so landing the sha would not be a fast-forward. The change parks.
	Moved Kind = "Moved"
	// Ruled: GitHub's rules or the key's permission refused it. The order holds, and so does every order after it.
	Ruled Kind = "Ruled"
	// Failed: anything else (the network, a sha GitHub lacks or that isn't a commit, a refusal nobody named). The order
	// holds.
	Failed Kind = "Failed"
)

// A Result is a push's kind and, unless it landed, what git said.
type Result struct {
	Kind   Kind
	Detail string
}

// Push moves the branch from from, its tip as just read, to exactly sha, as a fast-forward or not at all. The sha is
// fetched into a ref of its own, refs/loom/land/<sha>, and that ref is what is pushed, so no local name that reads like
// the sha (a branch called <sha>) can stand in for it. The sha must be a commit that contains from, which makes the push
// a fast-forward; it then goes with --force-with-lease=<branch>:<from>, so GitHub takes it only while the branch is still
// at from, which refuses a branch that moved or was deleted since the read and never creates one. A push GitHub accepted
// has landed only when the branch reads back as the sha, or as a commit that contains it (another landing already on top).
func (hands Hands) Push(sha, from string) Result {
	local := "refs/loom/land/" + sha
	defer Git(hands.Repository, nil, "update-ref", "-d", local)
	if _, stderr, err := Git(hands.Repository, nil, "fetch", "-q", "--no-tags", "origin", "+"+sha+":"+local, from); err != nil {
		return Result{Failed, fmt.Sprintf("fetching %.12s: %s", sha, said("", stderr, err))}
	}
	if held, _, err := Git(hands.Repository, nil, "rev-parse", "--verify", "-q", local); err != nil || strings.TrimSpace(held) != sha {
		return Result{Failed, fmt.Sprintf("%s doesn't hold %s after the fetch", local, sha)}
	}
	if kind, _, _ := Git(hands.Repository, nil, "cat-file", "-t", local); strings.TrimSpace(kind) != "commit" {
		return Result{Failed, fmt.Sprintf("%s is a %s, not a commit", sha, strings.TrimSpace(kind))}
	}
	_, stderr, err := Git(hands.Repository, nil, "merge-base", "--is-ancestor", from, local)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return Result{Moved, fmt.Sprintf("%s is at %.12s, which %.12s doesn't contain", hands.Branch, from, sha)}
	}
	if err != nil {
		return Result{Failed, fmt.Sprintf("is %.12s in %.12s: %s", from, sha, said("", stderr, err))}
	}
	stdout, stderr, err := Git(hands.Repository, nil, "push", "--porcelain", "--force-with-lease="+hands.Ref()+":"+from, "origin", local+":"+hands.Ref())
	if err != nil {
		return Result{Classify(stdout, stderr, hands.Ref()), said(stdout, stderr, err)}
	}
	if tip := hands.Tip(); tip != sha && !hands.contains(tip, sha) {
		return Result{Failed, fmt.Sprintf("the push was answered, but %s reads as %q, which isn't %s and doesn't contain it", hands.Ref(), tip, sha)}
	}
	return Result{Kind: Landed}
}

// contains is whether tip, a commit on GitHub, has sha in its history: fetched into the clone, then asked of git. A tip
// git can't fetch or answer for doesn't contain it.
func (hands Hands) contains(tip, sha string) bool {
	if !shaPattern.MatchString(tip) {
		return false
	}
	if _, _, err := Git(hands.Repository, nil, "fetch", "-q", "--no-tags", "origin", tip); err != nil {
		return false
	}
	_, _, err := Git(hands.Repository, nil, "merge-base", "--is-ancestor", sha, tip)
	return err == nil
}

// Sweep deletes every refs/loom/land/ ref the lander's clone holds, and says how many: each push deletes its own when it
// returns, but a pass systemd killed mid-push never does. A pass sweeps first, under the lock, so no other pass's ref is
// in flight.
func (hands Hands) Sweep() (int, error) {
	stdout, stderr, err := Git(hands.Repository, nil, "for-each-ref", "--format=%(refname)", "refs/loom/land/")
	if err != nil {
		return 0, fmt.Errorf("listing refs/loom/land/: %s", said("", stderr, err))
	}
	swept := 0
	for _, ref := range strings.Fields(stdout) {
		if _, stderr, err := Git(hands.Repository, nil, "update-ref", "-d", ref); err != nil {
			return swept, fmt.Errorf("deleting %s: %s", ref, said("", stderr, err))
		}
		swept++
	}
	return swept, nil
}

// said is what a refused push said, as one line of at most 300 bytes: git's refused ref lines, then what the remote and
// ssh said, git's hints left out; or the error when git said nothing.
func said(stdout, stderr string, err error) string {
	kept := []string{}
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "!") {
			kept = append(kept, strings.Join(strings.Fields(line), " "))
		}
	}
	for _, line := range strings.Split(stderr, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "hint:") {
			kept = append(kept, line)
		}
	}
	text := strings.Join(kept, " | ")
	if text == "" {
		return err.Error()
	}
	if len(text) > 300 {
		text = text[:300] + "..."
	}
	return text
}

// A ref line of `git push --porcelain` that git itself refused because the branch isn't where the push expected it:
// "!", the refspec, then "[rejected]" and "(stale info)" when the lease no longer holds (the branch moved or is gone),
// "(non-fast-forward)" or "(fetch first)".
var movedLine = regexp.MustCompile(`(?m)^!\t\S+\t\[rejected\] \((stale info|non-fast-forward|fetch first)\)$`)

// The reason on a ref line GitHub refused: "!", the refspec, "[remote rejected]" and the reason in parentheses.
var remoteRejected = regexp.MustCompile(`(?m)^!\t\S+\t\[remote rejected\] \((.*)\)$`)

// What GitHub, or ssh in front of it, says when a rule or a permission refuses a push: a ruleset (GH013), a protected
// branch (GH006), a hook that declined, a key that is read-only, deleted or not allowed.
var ruledText = regexp.MustCompile(`(?i)\bGH0\d\d\b|repository rule|rule violation|protected branch|declined|read[- ]only|permission|denied|not allowed|unauthori[sz]ed|forbidden|deploy key`)

// remoteSaid is what the far side said of a refused push, and nothing git wrote of its own: the "remote:" lines, the
// reason on a "[remote rejected]" ref line, GitHub's "ERROR:" lines over ssh, and ssh's own "Permission denied". The
// branch's name is taken out of it, so a branch called denied-x never reads as a refusal by permission.
func remoteSaid(stdout, stderr, ref string) string {
	kept := []string{}
	for _, match := range remoteRejected.FindAllStringSubmatch(stdout, -1) {
		kept = append(kept, match[1])
	}
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "remote:") || strings.HasPrefix(line, "ERROR:") || strings.Contains(line, ": Permission denied (") {
			kept = append(kept, line)
		}
	}
	text := strings.Join(kept, "\n")
	return strings.ReplaceAll(strings.ReplaceAll(text, ref, "<branch>"), strings.TrimPrefix(ref, "refs/heads/"), "<branch>")
}

// Classify reads a refused push of ref. It is Ruled when the far side named a rule or a permission. It is Moved only
// when git's own ref line says the branch wasn't where the push expected it and GitHub refused nothing itself; a
// "[remote rejected]" line is GitHub's answer, never the branch moving, so a ruleset that misfires holds every change
// instead of parking it. Failed is everything nobody named, and it holds too.
func Classify(stdout, stderr, ref string) Kind {
	switch {
	case ruledText.MatchString(remoteSaid(stdout, stderr, ref)):
		return Ruled
	case movedLine.MatchString(stdout) && !strings.Contains(stdout, "[remote rejected]"):
		return Moved
	default:
		return Failed
	}
}

// An Order is a landing order as Queue serves it at GET /landings, in line order.
type Order struct {
	Change string `json:"change"`
	Future string `json:"future"`
	Base   string `json:"base"`
	Owner  string `json:"owner"`
	Run    string `json:"run"`
}

var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var changePattern = regexp.MustCompile(`^chg_[0-9a-z]{26}$`)

// A Pass counts what one pass did: changes landed and parked, orders held, and whether the queue couldn't be read.
type Pass struct {
	Landed, Parked, Held int
	Unread               bool
}

// Land is one pass over the landing orders, after sweeping the landing refs a killed pass left. Each is checked to be a change and a sha before git sees it, so an order
// can never name a ref, an option or a forced refspec; the branch's tip is read before each push and reported as the
// main it moved from, or the main a refusal found. A branch that already holds the order's sha (at it, or with other
// landings on top) is a landing whose report was lost: before any push, it is reported again as main and from both the
// sha, which Queue takes as the same landing, so it never parks. A report the queue doesn't take holds the order, so
// the pass says so.
func Land(queue Queue, hands Hands, log func(string)) Pass {
	pass := Pass{}
	if swept, err := hands.Sweep(); err != nil {
		log(fmt.Sprintf("sweeping the landing refs a killed pass left: %v", err))
	} else if swept > 0 {
		log(fmt.Sprintf("swept %d landing refs a killed pass left", swept))
	}
	status, answer := call(queue, "GET", "/landings", nil)
	var listed struct {
		Landings []Order `json:"landings"`
	}
	if status != 200 || json.Unmarshal(answer, &listed) != nil {
		log(fmt.Sprintf("landings: %d %s", status, answerText(answer)))
		pass.Unread = true
		return pass
	}
	report := func(order Order, body map[string]string, done string) bool {
		status, answer := call(queue, "POST", "/landings/"+order.Change, body)
		if status != 200 {
			log(fmt.Sprintf("holding %s: %s, and the queue didn't take the report: %d %s", order.Change, done, status, answerText(answer)))
			pass.Held++
			return false
		}
		log(fmt.Sprintf("%s; %d %s", done, status, answerText(answer)))
		return true
	}
	for _, order := range listed.Landings {
		if !changePattern.MatchString(order.Change) || !shaPattern.MatchString(order.Future) {
			log(fmt.Sprintf("holding %q: its future %q isn't a 40-hex sha, or its change isn't a change; nothing pushed", order.Change, order.Future))
			pass.Held++
			continue
		}
		from := hands.Tip()
		if from == "" {
			log(fmt.Sprintf("can't read %s; holding %s", hands.Ref(), order.Change))
			pass.Held++
			return pass
		}
		if from == order.Future || hands.contains(from, order.Future) {
			if report(order, map[string]string{"main": order.Future, "from": order.Future, "landed": order.Future},
				fmt.Sprintf("landed %s: %s at %.12s already holds %.12s, a landing whose report was lost", order.Change, hands.Branch, from, order.Future)) {
				pass.Landed++
			}
			continue
		}
		result := hands.Push(order.Future, from)
		if result.Kind == Moved && hands.Tip() == "" {
			result = Result{Failed, fmt.Sprintf("%s is gone: %s", hands.Ref(), result.Detail)}
		}
		switch result.Kind {
		case Landed:
			if report(order, map[string]string{"main": order.Future, "from": from, "landed": order.Future},
				fmt.Sprintf("landed %s: %s %.12s..%.12s", order.Change, hands.Branch, from, order.Future)) {
				pass.Landed++
			}
		case Moved:
			refused := "not a fast-forward of " + hands.Branch + ": " + result.Detail
			if report(order, map[string]string{"refused": refused, "main": from},
				fmt.Sprintf("refused %s on %s %.12s: %s", order.Change, hands.Branch, from, refused)) {
				pass.Parked++
			}
		case Ruled:
			log(fmt.Sprintf("HELD %s and every order after it: GitHub refused the push to %s by a rule or the key's permission, which is no change's fault, so nothing parks: %s", order.Change, hands.Ref(), result.Detail))
			pass.Held++
			return pass
		default:
			log(fmt.Sprintf("holding %s: %s", order.Change, result.Detail))
			pass.Held++
		}
	}
	return pass
}

// Once is one whole pass: blocks, then the landing orders. Whatever the block code does (an answer it can't read, a git
// timeout, a panic), the branch still lands: its failure is logged and the landing pass always runs (Loom, 01:11Z).
func Once(queue Queue, hands Hands, builder Builder, log func(string)) Pass {
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log(fmt.Sprintf("blocks failed, landing anyway: %v", recovered))
			}
		}()
		BuildBlocks(queue, builder, log)
	}()
	return Land(queue, hands, log)
}
