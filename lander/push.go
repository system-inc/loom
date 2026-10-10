package lander

import (
	"bytes"
	"context"
	"encoding/json"
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
	// Moved: git refused the push as not a fast-forward, since the branch moved past the sha's base. The change parks.
	Moved Kind = "Moved"
	// Ruled: GitHub's rules or the key's permission refused it. The order holds, and so does every order after it.
	Ruled Kind = "Ruled"
	// Failed: anything else (the network, a sha GitHub lacks, a refusal nobody named). The order holds.
	Failed Kind = "Failed"
)

// A Result is a push's kind and, unless it landed, what git said.
type Result struct {
	Kind   Kind
	Detail string
}

// Push fetches sha into the lander's clone and pushes exactly it to the branch, without force.
func (hands Hands) Push(sha string) Result {
	if _, stderr, err := Git(hands.Repository, nil, "fetch", "-q", "--no-tags", "origin", sha); err != nil {
		return Result{Failed, fmt.Sprintf("fetching %.12s: %s", sha, said("", stderr, err))}
	}
	stdout, stderr, err := Git(hands.Repository, nil, "push", "--porcelain", "origin", sha+":"+hands.Ref())
	if err == nil {
		return Result{Kind: Landed}
	}
	return Result{Classify(stdout, stderr), said(stdout, stderr, err)}
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

// A ref line of `git push --porcelain` that git itself refused because the remote moved: "!", the refspec, then
// "[rejected] (non-fast-forward)", or "(fetch first)" when the clone hasn't seen the remote's new tip.
var movedLine = regexp.MustCompile(`(?m)^!\t\S+\t\[rejected\] \((non-fast-forward|fetch first)\)$`)

// The lines of a push's output that name the remote, which say nothing of why it refused.
var remoteLine = regexp.MustCompile(`(?m)^To .*$|failed to push some refs to .*$`)

// What GitHub, or ssh in front of it, says when a rule or a permission refuses a push: a ruleset (GH013), a protected
// branch (GH006), a hook that declined, a key that is read-only, deleted or not allowed.
var ruledText = regexp.MustCompile(`(?i)\bGH0\d\d\b|repository rule|rule violation|protected branch|declined|read[- ]only|permission|denied|not allowed|unauthori[sz]ed|forbidden|deploy key`)

// Classify reads a refused push. It is Moved only when git's own ref line says not a fast-forward and nothing in the
// output names a rule or a permission; a "[remote rejected]" line is GitHub's answer, never main moving, so a ruleset
// that misfires holds every change instead of parking it. Failed is everything nobody named, and it holds too.
func Classify(stdout, stderr string) Kind {
	// The remote's address is no answer of GitHub's, so a word in it (a path, a host alias) never reads as a refusal.
	output := remoteLine.ReplaceAllString(stdout+"\n"+stderr, "")
	switch {
	case ruledText.MatchString(output):
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

// Land is one pass over the landing orders. Each is checked to be a change and a sha before git sees it, so an order
// can never name a ref, an option or a forced refspec; the branch's tip is read before each push and reported as the
// main it moved from, or the main a refusal found.
func Land(queue Queue, hands Hands, log func(string)) Pass {
	pass := Pass{}
	status, answer := call(queue, "GET", "/landings", nil)
	var listed struct {
		Landings []Order `json:"landings"`
	}
	if status != 200 || json.Unmarshal(answer, &listed) != nil {
		log(fmt.Sprintf("landings: %d %s", status, answerText(answer)))
		pass.Unread = true
		return pass
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
		result := hands.Push(order.Future)
		switch result.Kind {
		case Landed:
			status, answer := call(queue, "POST", "/landings/"+order.Change, map[string]string{"main": order.Future, "from": from, "landed": order.Future})
			log(fmt.Sprintf("landed %s: %s %.12s..%.12s, %d %s", order.Change, hands.Branch, from, order.Future, status, answerText(answer)))
			pass.Landed++
		case Moved:
			refused := "not a fast-forward of " + hands.Branch + ": " + result.Detail
			status, answer := call(queue, "POST", "/landings/"+order.Change, map[string]string{"refused": refused, "main": from})
			log(fmt.Sprintf("refused %s on %s %.12s: %s; %d %s", order.Change, hands.Branch, from, refused, status, answerText(answer)))
			pass.Parked++
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
