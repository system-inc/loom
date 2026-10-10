package release

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Commands are the watcher's steps on Workshop: git in the configured clone, and the clone's own updater/publish.sh
// and updater/upload.sh, run as a person runs them (docs/updater.md), their output to log. The scripts come from the
// clone's checkout, not from the commit being released, so a commit can't change how it is itself published.
func Commands(config Config, log io.Writer) Steps {
	git := func(callContext context.Context, arguments ...string) (string, error) {
		command := exec.CommandContext(callContext, "git", append([]string{"-C", config.Repository}, arguments...)...)
		command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		err := command.Run()
		if err != nil {
			return stdout.String(), fmt.Errorf("git %s: %w: %s", strings.Join(arguments, " "), err, strings.TrimSpace(stderr.String()))
		}
		return strings.TrimSpace(stdout.String()), nil
	}
	// A script takes <out>/.release.lock itself (updater/release-lock.sh); run under the pass's own hold, it is handed
	// that hold as its descriptor 3.
	script := func(callContext context.Context, name string, arguments ...string) error {
		command := exec.CommandContext(callContext, filepath.Join(config.Repository, "updater", name), arguments...)
		command.Stdout, command.Stderr = log, log
		if lock := held(callContext); lock != nil {
			command.ExtraFiles = []*os.File{lock}
			command.Env = append(os.Environ(), "LOOM_RELEASE_LOCK_FD=3")
		}
		if err := command.Run(); err != nil {
			return fmt.Errorf("updater/%s %s: %w", name, strings.Join(arguments, " "), err)
		}
		return nil
	}
	return Steps{
		Head: func(callContext context.Context) (string, error) {
			tracking := "refs/remotes/" + config.Remote + "/" + config.Branch
			if _, err := git(callContext, "fetch", "--quiet", config.Remote, "+refs/heads/"+config.Branch+":"+tracking); err != nil {
				return "", err
			}
			return git(callContext, "rev-parse", "--verify", tracking+"^{commit}")
		},
		Descends: func(callContext context.Context, ancestor, commit string) (bool, error) {
			_, err := git(callContext, "merge-base", "--is-ancestor", ancestor, commit)
			var exit *exec.ExitError
			if errors.As(err, &exit) && exit.ExitCode() == 1 {
				return false, nil
			}
			return err == nil, err
		},
		Order: func(callContext context.Context, commit string) (string, error) {
			if _, err := git(callContext, "cat-file", "-e", commit+":"+OrderFile); err != nil {
				return "", nil
			}
			return git(callContext, "show", commit+":"+OrderFile)
		},
		Publish: func(callContext context.Context, commit, canary string) error {
			if canary != "" {
				return script(callContext, "publish.sh", "--canary", canary, commit, config.Out)
			}
			return script(callContext, "publish.sh", commit, config.Out)
		},
		Promote: func(commit string) error { return Promote(config.Out, commit) },
		Upload: func(callContext context.Context, current string) error {
			if current != "" {
				return script(callContext, "upload.sh", "--current", current, config.Out, config.Destination)
			}
			return script(callContext, "upload.sh", config.Out, config.Destination)
		},
		Published: func(callContext context.Context) (Manifest, error) {
			return FetchManifest(callContext, config.Base, true)
		},
	}
}

// FetchManifest is <base>/current.txt, what every box reads; fresh asks past any cache in front of the store.
func FetchManifest(callContext context.Context, base string, fresh bool) (Manifest, error) {
	location := strings.TrimSuffix(base, "/") + "/current.txt"
	if fresh {
		location += fmt.Sprintf("?read=%d", time.Now().UnixNano())
	}
	request, err := http.NewRequestWithContext(callContext, http.MethodGet, location, nil)
	if err != nil {
		return Manifest{}, err
	}
	request.Header.Set("Cache-Control", "no-cache")
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		return Manifest{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err == nil && response.StatusCode != http.StatusOK {
		err = errors.New(response.Status)
	}
	if err != nil {
		return Manifest{}, fmt.Errorf("%s/current.txt: %w", base, err)
	}
	manifest, err := ParseManifest(string(body))
	if err != nil {
		return Manifest{}, fmt.Errorf("%s/current.txt: %w", base, err)
	}
	return manifest, nil
}

// ManifestPath is where publish.sh keeps a commit's own manifest: <out>/manifests/<commit>.txt.
func ManifestPath(out, commit string) string {
	return filepath.Join(out, "manifests", commit+".txt")
}

// OwnManifest is the commit's own manifest, refused unless it names that commit alone.
func OwnManifest(out, commit string) ([]byte, error) {
	content, err := os.ReadFile(ManifestPath(out, commit))
	if err != nil {
		return nil, err
	}
	if manifest, err := ParseManifest(string(content)); err != nil || !Alone(manifest, commit) {
		return nil, fmt.Errorf("manifests/%s.txt isn't that commit's manifest alone (%v)", commit, err)
	}
	return content, nil
}

// Promote makes the commit's own manifest, which publish.sh kept at manifests/<commit>.txt, the out directory's
// current.txt: every box follows it, the canary line gone. It is how a canary is promoted and how one is ended, once
// the boxes read it.
func Promote(out, commit string) error {
	content, err := OwnManifest(out, commit)
	if err != nil {
		return err
	}
	partial := filepath.Join(out, ".current.txt.promote")
	if err := os.WriteFile(partial, content, 0o644); err != nil {
		return err
	}
	return os.Rename(partial, filepath.Join(out, "current.txt"))
}
