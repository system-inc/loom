package queuebridge

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// A Pin is a gitlink in a change's tree, at any depth (adamic's cohere, cohere's TypeScript): its path from adamic's
// root, the url its parent's .gitmodules names for it (as PinUrl reads it), the commit it pins, and whether a machine
// with no key can fetch that commit from that url, with the reason when it can't (#yt2jw5q). A runner fetches every
// pin keyless, so a pin to a commit that was never pushed, or that sits behind a key, would pass every rule and fail
// later on every runner.
type Pin struct {
	Path      string `json:"path"`
	Url       string `json:"url"`
	Sha       string `json:"sha"`
	Fetchable bool   `json:"fetchable"`
	Reason    string `json:"reason,omitempty"`
}

// PinDepth is how deep pins are proven: adamic, cohere, TypeScript, and room for one more. A pin nested deeper is
// listed unfetchable, named, never fetched.
const PinDepth = 4

// gitlinkPattern is one `git ls-tree -r` line naming a gitlink: the mode, the commit and the path.
var gitlinkPattern = regexp.MustCompile(`^160000 commit ([0-9a-f]{40})\t(.+)$`)

// pinUrlPattern is the one rule for a pin's url, shared with the runner's prepare.sh (its pinUrl, which the table test
// runs beside this): a repository on github.com over https, or git@github.com: read as https, owner/name and nothing
// else. Any other host (a house address among them), protocol, credential in the url or relative path is refused.
var pinUrlPattern = regexp.MustCompile(`^(https://github\.com/|git@github\.com:)([A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9._-]+)$`)

// PinUrl is the https url a runner fetches a pin from, or "" and the reason no runner can, in prepare.sh's words to the
// letter (the pin's url field names the url itself).
func PinUrl(written string) (string, string) {
	found := pinUrlPattern.FindStringSubmatch(written)
	if found == nil || strings.Contains(found[2], "..") {
		return "", "it isn't a github.com repository over https (or git@github.com:, read as https), the only place a runner fetches a pin from"
	}
	return "https://github.com/" + found[2], ""
}

// gitlinks are the pins in a commit's tree as (sha, path) pairs, with the url its .gitmodules names for each path; a
// path .gitmodules doesn't name has url "".
func gitlinks(repository, commit string) ([][2]string, map[string]string, error) {
	listed, stderr, code, err := run(5*time.Minute, "", keyless(), nil, "git", gitArguments(repository, "ls-tree", "-r", commit)...)
	if err != nil || code != 0 {
		return nil, nil, &GitError{fmt.Sprintf("git ls-tree %s: %v %s", commit[:min(12, len(commit))], err, first(strings.TrimSpace(stderr), 300))}
	}
	var links [][2]string
	for _, line := range strings.Split(listed, "\n") {
		if found := gitlinkPattern.FindStringSubmatch(line); found != nil {
			links = append(links, [2]string{found[1], found[2]})
		}
	}
	urls := map[string]string{}
	if len(links) == 0 {
		return nil, urls, nil
	}
	// git config exits 1 when nothing matches, and the blob may be missing; either way no path is named.
	named, _, code, err := run(5*time.Minute, "", keyless(), nil, "git", gitArguments(repository, "config", "--blob", commit+":.gitmodules", "--get-regexp", `^submodule\..*\.(path|url)$`)...)
	if err != nil {
		return nil, nil, &GitError{fmt.Sprintf("git config --blob %s:.gitmodules: %v", commit[:min(12, len(commit))], err)}
	}
	if code == 0 {
		paths, addresses := map[string]string{}, map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(named), "\n") {
			key, value, _ := strings.Cut(line, " ")
			name, isPath := strings.CutSuffix(strings.TrimPrefix(key, "submodule."), ".path")
			if isPath {
				paths[name] = value
			} else if name, isUrl := strings.CutSuffix(strings.TrimPrefix(key, "submodule."), ".url"); isUrl {
				addresses[name] = value
			}
		}
		for name, where := range paths {
			urls[where] = addresses[name]
		}
	}
	return links, urls, nil
}

// notPushed and notPublic are what GitHub says when no machine without a key can have the commit: it isn't on the
// repository (never pushed, or only to a ref it doesn't keep), or the repository isn't public (not found, a login
// asked for). Anything else, throttling (403, 429), GitHub's 5xx, DNS or a timeout among it, says nothing about the
// pin, so the facts wait instead and the change is never refused on a guess.
var (
	notPushed = []string{"not our ref", "unadvertised object"}
	notPublic = []string{"Repository not found", "' not found", "could not read Username", "terminal prompts disabled", "Authentication failed", "returned error: 401", "returned error: 404"}
)

// fetchPin fetches sha from address (an https github.com url, PinUrl's) the way a runner would, keyless, shallow and
// with no blobs, into a fresh scratch store whose blobs are read on demand from the same remote. It answers the store
// (for the pins under this one), "" and our reason when no keyless machine can have the commit, or a GitError when the
// remote said nothing about it.
func (clone Clone) fetchPin(scratch, address, sha string) (string, string, error) {
	store, err := os.MkdirTemp(scratch, "pin-")
	if err != nil {
		return "", "", err
	}
	from := address
	if clone.mirror != nil {
		from = clone.mirror(address)
	}
	for _, arguments := range [][]string{{"init", "-q", "--bare", store}, {"-C", store, "config", "core.repositoryformatversion", "1"},
		{"-C", store, "config", "extensions.partialClone", "origin"}, {"-C", store, "remote", "add", "origin", from},
		{"-C", store, "config", "remote.origin.promisor", "true"}, {"-C", store, "config", "remote.origin.partialclonefilter", "blob:none"}} {
		if _, stderr, code, err := run(time.Minute, "", keyless(), nil, "git", append(append([]string{}, keylessSettings...), arguments...)...); err != nil || code != 0 {
			return "", "", &GitError{fmt.Sprintf("making a scratch store: git %s: %v %s", arguments[0], err, first(strings.TrimSpace(stderr), 300))}
		}
	}
	_, stderr, code, err := run(2*time.Minute, "", keyless(), nil, "git", gitArguments(store, "fetch", "-q", "--depth=1", "--filter=blob:none", "--no-tags", "origin", sha)...)
	if err != nil {
		return "", "", &GitError{fmt.Sprintf("fetching %s from %s: %v", sha[:12], address, err)}
	}
	if code == 0 {
		return store, "", nil
	}
	said := strings.TrimSpace(stderr)
	for _, words := range notPushed {
		if strings.Contains(said, words) {
			return "", fmt.Sprintf("%s doesn't give %s to a fetch with no key: the commit isn't pushed there", address, sha[:12]), nil
		}
	}
	for _, words := range notPublic {
		if strings.Contains(said, words) {
			return "", fmt.Sprintf("%s isn't a public repository: a fetch with no key can't read it", address), nil
		}
	}
	return "", "", &GitError{fmt.Sprintf("fetching %s from %s exited %d: %s", sha[:12], address, code, first(said[max(0, len(said)-300):], 300))}
}

// PinsOf are every pin in sha's tree, recursively, each proven fetchable or not by a keyless fetch into a scratch store
// removed after. A GitError when git or GitHub couldn't say (slow, down, or throttling): the facts then wait, so a
// change is never refused or passed on a guess.
func (clone Clone) PinsOf(sha string) ([]Pin, error) {
	scratch, err := os.MkdirTemp("", "loom-pins-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch)
	pins := []Pin{}
	var follow func(repository, commit, prefix string, depth int) error
	follow = func(repository, commit, prefix string, depth int) error {
		links, urls, err := gitlinks(repository, commit)
		if err != nil {
			return err
		}
		for _, link := range links {
			pin := Pin{Path: prefix + link[1], Url: urls[link[1]], Sha: link[0]}
			store := ""
			if depth > PinDepth {
				pin.Reason = fmt.Sprintf("it is nested %d submodules deep, past the %d the bridge proves", depth, PinDepth)
			} else if address, refused := PinUrl(pin.Url); refused != "" {
				pin.Reason = refused
			} else {
				pin.Url = address
				if store, pin.Reason, err = clone.fetchPin(scratch, address, pin.Sha); err != nil {
					return err
				}
			}
			pin.Fetchable = pin.Reason == ""
			pins = append(pins, pin)
			if pin.Fetchable {
				if err := follow(store, pin.Sha, pin.Path+"/", depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := follow(clone.Repository, sha, "", 1); err != nil {
		return nil, err
	}
	return pins, nil
}
