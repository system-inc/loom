package queuebridge

import (
	"fmt"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"time"
)

// A Pin is a gitlink in a change's tree, at any depth (adamic's cohere, cohere's TypeScript): its path from adamic's
// root, the url its parent's .gitmodules names for it, the commit it pins, and whether a machine with no key can fetch
// that commit from that url, with the reason when it can't (#yt2jw5q). A runner fetches every pin keyless, so a pin to a
// commit that was never pushed, or that sits behind a key, would pass every rule and fail later on every runner.
type Pin struct {
	Path      string `json:"path"`
	Url       string `json:"url"`
	Sha       string `json:"sha"`
	Fetchable bool   `json:"fetchable"`
	Reason    string `json:"reason,omitempty"`
}

// pinDepth bounds how deep pins are followed: adamic, cohere, TypeScript, and room for one more.
const pinDepth = 4

// gitlinkPattern is one `git ls-tree -r` line naming a gitlink: the mode, the commit and the path.
var gitlinkPattern = regexp.MustCompile(`^160000 commit ([0-9a-f]{40})\t(.+)$`)

// keylessReasons are what a remote says when a machine with no key can't have the commit: it was never pushed (not our
// ref, an unadvertised object), or the repository isn't public (not found, a prompt for a name). Any other failure (DNS,
// a timeout, GitHub's 5xx) says nothing about the pin, so the facts wait instead.
var keylessReasons = []string{"not our ref", "unadvertised object", "not found", "could not read Username",
	"terminal prompts disabled", "Authentication failed", "returned error: 401", "returned error: 403", "returned error: 404"}

// sshPattern is a url only a key can read: ssh://, git+ssh://, ssh+git:// or scp-like user@host:path.
var sshPattern = regexp.MustCompile(`^((git\+)?ssh(\+git)?://|[^/:@]+@[^/:]+:)`)

// gitlinks are the pins in a commit's tree as (sha, path) pairs, with the url its .gitmodules names for each path; a
// path .gitmodules doesn't name has url "".
func gitlinks(repository, commit string, environment []string) ([][2]string, map[string]string, error) {
	listed, stderr, code, err := run(5*time.Minute, "", environment, nil, "git", "-C", repository, "ls-tree", "-r", commit)
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
	named, _, code, err := run(5*time.Minute, "", environment, nil, "git", "-C", repository, "config", "--blob", commit+":.gitmodules", "--get-regexp", `^submodule\..*\.(path|url)$`)
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

// resolveUrl is a .gitmodules url as git reads it: absolute as written, and ./ or ../ relative to the parent's url.
func resolveUrl(parent, written string) string {
	if !strings.HasPrefix(written, "./") && !strings.HasPrefix(written, "../") {
		return written
	}
	base, err := url.Parse(parent)
	if err != nil || base.Scheme == "" {
		return written
	}
	base.Path = path.Join(strings.TrimSuffix(base.Path, "/"), written)
	return base.String()
}

// keyless is the environment a runner's fetch has: no global or system git settings (no insteadOf rewriting https to
// ssh, no credential helper), no ~/.netrc (HOME is the scratch directory), no prompt and no ssh.
func keyless(scratch string) []string {
	return []string{"HOME=" + scratch, "XDG_CONFIG_HOME=" + scratch, "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/false", "SSH_ASKPASS=/bin/false", "GIT_SSH_COMMAND=/bin/false"}
}

// fetchPin fetches sha from address the way a runner would, keyless, shallow and with no blobs, into a fresh scratch
// store whose blobs are read on demand from the same remote. It answers the store (for the pins under this one), "" and
// a reason when no key-less machine can have the commit, or a GitError when the remote said nothing about it.
func fetchPin(scratch, address, sha string) (string, string, error) {
	if address == "" {
		return "", "its parent's .gitmodules names no url for it", nil
	}
	if sshPattern.MatchString(address) {
		return "", "its url is ssh, which needs a key a runner doesn't hold", nil
	}
	if parsed, err := url.Parse(address); err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return "", "its url isn't http(s), so a runner can't fetch it", nil
	}
	store, err := os.MkdirTemp(scratch, "pin-")
	if err != nil {
		return "", "", err
	}
	environment := keyless(scratch)
	for _, arguments := range [][]string{{"init", "-q", "--bare", store}, {"-C", store, "config", "core.repositoryformatversion", "1"},
		{"-C", store, "config", "extensions.partialClone", "origin"}, {"-C", store, "remote", "add", "origin", address},
		{"-C", store, "config", "remote.origin.promisor", "true"}, {"-C", store, "config", "remote.origin.partialclonefilter", "blob:none"}} {
		if _, stderr, code, err := run(time.Minute, "", environment, nil, "git", arguments...); err != nil || code != 0 {
			return "", "", &GitError{fmt.Sprintf("making a scratch store: git %s: %v %s", arguments[0], err, first(strings.TrimSpace(stderr), 300))}
		}
	}
	_, stderr, code, err := run(2*time.Minute, "", environment, nil, "git", "-C", store, "fetch", "-q", "--depth=1", "--filter=blob:none", "--no-tags", "origin", sha)
	if err != nil {
		return "", "", &GitError{fmt.Sprintf("fetching %s from %s: %v", sha[:12], address, err)}
	}
	if code == 0 {
		return store, "", nil
	}
	said := strings.TrimSpace(stderr)
	for _, reason := range keylessReasons {
		if strings.Contains(said, reason) {
			return "", fmt.Sprintf("a keyless fetch from %s refused it: %s", address, first(said[max(0, len(said)-200):], 200)), nil
		}
	}
	return "", "", &GitError{fmt.Sprintf("fetching %s from %s exited %d: %s", sha[:12], address, code, first(said[max(0, len(said)-300):], 300))}
}

// PinsOf are every pin in sha's tree, recursively, each proven fetchable or not by a keyless fetch into a scratch store
// removed after. A GitError when git or a remote couldn't say (GitHub slow or down): the facts then wait, so a change
// is never refused or passed on a guess.
func (clone Clone) PinsOf(sha string) ([]Pin, error) {
	origin, _ := clone.git([]string{"remote", "get-url", "origin"})
	scratch, err := os.MkdirTemp("", "loom-pins-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch)
	pins := []Pin{}
	var follow func(repository, commit, prefix, parentUrl string, depth int, environment []string) error
	follow = func(repository, commit, prefix, parentUrl string, depth int, environment []string) error {
		links, urls, err := gitlinks(repository, commit, environment)
		if err != nil {
			return err
		}
		for _, link := range links {
			pin := Pin{Path: prefix + link[1], Url: resolveUrl(parentUrl, urls[link[1]]), Sha: link[0]}
			store, reason, err := fetchPin(scratch, pin.Url, pin.Sha)
			if err != nil {
				return err
			}
			pin.Fetchable, pin.Reason = reason == "", reason
			pins = append(pins, pin)
			if pin.Fetchable && depth < pinDepth {
				if err := follow(store, pin.Sha, pin.Path+"/", pin.Url, depth+1, keyless(scratch)); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := follow(clone.Repository, sha, "", origin, 1, nil); err != nil {
		return nil, err
	}
	return pins, nil
}
