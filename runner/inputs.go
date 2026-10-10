package runner

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/system-inc/loom/protocol"
)

// storeAttempts is how many times a GET or PUT to the store is tried before the unit is broken.
const storeAttempts = 3

// fetchInputs fetches every input by its hash, verifies the bytes, and places them in the workspace. A
// blob two inputs share is fetched once. All writes go through an os.Root on the workspace, so nothing an
// earlier input placed (a symlink in an archive, say) can steer a later write outside it.
func (run *unitRun) fetchInputs(runContext context.Context) error {
	if len(run.unit.Inputs) == 0 {
		return nil
	}
	workspace, err := os.OpenRoot(run.workspace)
	if err != nil {
		return err
	}
	defer workspace.Close()
	staged := map[string]string{}
	for _, input := range run.unit.Inputs {
		blob, ok := staged[input.Sha256]
		if !ok {
			blob, err = run.fetchBlob(runContext, input.Sha256)
			if err != nil {
				return fmt.Errorf("input %s: %w", input.Path, err)
			}
			staged[input.Sha256] = blob
		}
		path := filepath.FromSlash(input.Path)
		if input.Archive == "tar" {
			err = unpackTar(workspace, path, blob)
		} else {
			err = placeFile(workspace, path, input.Mode, blob)
		}
		if err != nil {
			return fmt.Errorf("input %s: %w", input.Path, err)
		}
	}
	for _, blob := range staged {
		os.Remove(blob)
	}
	return nil
}

// fetchBlob downloads <store>/<sha256> into the staging area and returns its path there. The bytes are
// hashed as they arrive; a blob whose hash isn't the one asked for is deleted and refused, never retried,
// since a store that serves the wrong bytes once has nothing to offer a second time.
func (run *unitRun) fetchBlob(runContext context.Context, hash string) (string, error) {
	path := filepath.Join(run.staging, hash)
	var lastError error
	for attempt := range storeAttempts {
		if attempt > 0 {
			sleepFor(runContext, attempt)
		}
		got, retry, err := run.download(runContext, storeUrl(run.unit.Store.Url, hash), path)
		if err == nil && got == hash {
			return path, nil
		}
		os.Remove(path)
		if err == nil {
			return "", fmt.Errorf("refused: the store's bytes hash to %s, not %s", got, hash)
		}
		lastError = err
		if !retry || runContext.Err() != nil {
			break
		}
	}
	return "", lastError
}

// sleepFor waits before a store request's next attempt, longer each time, or until the unit is stopped.
func sleepFor(runContext context.Context, attempt int) {
	select {
	case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
	case <-runContext.Done():
	}
}

// download makes one GET of a blob into path and returns the sha256 of what arrived. retry says whether a
// failure is worth another attempt.
func (run *unitRun) download(runContext context.Context, url string, path string) (got string, retry bool, err error) {
	request, err := http.NewRequestWithContext(runContext, http.MethodGet, url, nil)
	if err != nil {
		return "", false, err
	}
	if run.unit.Token != "" {
		request.Header.Set("Authorization", "Bearer "+run.unit.Token)
	}
	response, err := run.options.Client.Do(request)
	if err != nil {
		return "", true, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		retry := response.StatusCode/100 == 5 || response.StatusCode == http.StatusTooManyRequests
		return "", retry, fmt.Errorf("GET %s: %s", request.URL.Redacted(), response.Status)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", false, err
	}
	hasher := sha256.New()
	_, copyError := io.Copy(io.MultiWriter(file, hasher), response.Body)
	closeError := file.Close()
	if copyError != nil {
		return "", true, fmt.Errorf("GET %s: %w", request.URL.Redacted(), copyError)
	}
	if closeError != nil {
		return "", false, closeError
	}
	return hex.EncodeToString(hasher.Sum(nil)), false, nil
}

func storeUrl(store string, hash string) string {
	return strings.TrimSuffix(store, "/") + "/" + hash
}

// placeFile copies a verified blob to path in the workspace with the input's mode. An input never
// replaces a file already there.
func placeFile(workspace *os.Root, path string, modeText string, blob string) error {
	mode, err := protocol.ParseMode(modeText)
	if err != nil {
		return err
	}
	if err := workspace.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	source, err := os.Open(blob)
	if err != nil {
		return err
	}
	defer source.Close()
	destination, err := workspace.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(destination, source); err != nil {
		destination.Close()
		return err
	}
	if err := destination.Close(); err != nil {
		return err
	}
	return workspace.Chmod(path, mode) // the umask may have taken bits OpenFile was given
}

// unpackTar unpacks a tar archive (gzipped or not) at path. An entry that would land outside path is
// refused: a name that climbs out or is absolute, a symlink whose target leaves, a hard link to a name
// outside, and any entry type that isn't a file, directory or link. The names are checked before anything
// is written, writes go through an os.Root on path, and once every entry is down each symlink is resolved
// for real, which catches the chains a name alone can't show (a link to "d/.." where d links to ".").
func unpackTar(workspace *os.Root, path string, blob string) error {
	if err := workspace.MkdirAll(path, 0o755); err != nil {
		return err
	}
	destination, err := workspace.OpenRoot(path)
	if err != nil {
		return err
	}
	defer destination.Close()
	file, err := os.Open(blob)
	if err != nil {
		return err
	}
	defer file.Close()
	buffered := bufio.NewReader(file)
	var reader io.Reader = buffered
	if magic, err := buffered.Peek(2); err == nil && magic[0] == 0x1f && magic[1] == 0x8b {
		decompressor, err := gzip.NewReader(buffered)
		if err != nil {
			return err
		}
		defer decompressor.Close()
		reader = decompressor
	}

	type directoryMode struct {
		name string
		mode os.FileMode
	}
	var directoryModes []directoryMode
	var symlinks []string
	archive := tar.NewReader(reader)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("reading the archive: %w", err)
		}
		name := filepath.FromSlash(header.Name)
		if !filepath.IsLocal(name) {
			return fmt.Errorf("refused: archive entry %q escapes %s", header.Name, path)
		}
		name = filepath.Clean(name)
		mode := header.FileInfo().Mode().Perm()
		switch header.Typeflag {
		case tar.TypeDir:
			if err := destination.MkdirAll(name, 0o755); err != nil {
				return err
			}
			directoryModes = append(directoryModes, directoryMode{name, mode})
		case tar.TypeReg:
			if err := destination.MkdirAll(filepath.Dir(name), 0o755); err != nil {
				return err
			}
			entry, err := destination.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(entry, archive); err != nil {
				entry.Close()
				return err
			}
			if err := entry.Close(); err != nil {
				return err
			}
			if err := destination.Chmod(name, mode); err != nil {
				return err
			}
		case tar.TypeSymlink:
			target := filepath.FromSlash(header.Linkname)
			if filepath.IsAbs(target) || !filepath.IsLocal(filepath.Join(filepath.Dir(name), target)) {
				return fmt.Errorf("refused: archive symlink %q points to %q, outside %s", header.Name, header.Linkname, path)
			}
			if err := destination.MkdirAll(filepath.Dir(name), 0o755); err != nil {
				return err
			}
			if err := destination.Symlink(target, name); err != nil {
				return err
			}
			symlinks = append(symlinks, name)
		case tar.TypeLink:
			target := filepath.FromSlash(header.Linkname)
			if !filepath.IsLocal(target) {
				return fmt.Errorf("refused: archive hard link %q points to %q, outside %s", header.Name, header.Linkname, path)
			}
			if err := destination.MkdirAll(filepath.Dir(name), 0o755); err != nil {
				return err
			}
			if err := destination.Link(filepath.Clean(target), name); err != nil {
				return err
			}
		case tar.TypeXGlobalHeader:
		default:
			return fmt.Errorf("refused: archive entry %q has type %q, which a unit's input can't hold", header.Name, header.Typeflag)
		}
	}

	root, err := filepath.EvalSymlinks(filepath.Join(workspace.Name(), path))
	if err != nil {
		return err
	}
	for _, name := range symlinks {
		resolved, err := filepath.EvalSymlinks(filepath.Join(root, name))
		if err != nil {
			continue // dangling, and its target was checked by name
		}
		if !inside(root, resolved) {
			return fmt.Errorf("refused: archive symlink %q resolves to %s, outside %s", name, resolved, path)
		}
	}
	for index := len(directoryModes) - 1; index >= 0; index-- {
		if err := destination.Chmod(directoryModes[index].name, directoryModes[index].mode); err != nil {
			return err
		}
	}
	return nil
}

// inside reports whether path is root or below it. Both must already be resolved.
func inside(root string, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && filepath.IsLocal(relative)
}
