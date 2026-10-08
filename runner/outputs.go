package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/system-inc/loom/protocol"
)

// uploadOutputs hashes and uploads every file the unit's output globs match, emitting uploaded for each,
// and returns the status the outputs leave the unit in. A glob that matches no file, or a match that
// resolves outside the workspace, is the unit's doing: failed, since it didn't produce what it declared.
// A file the store won't take is the runner's: broken. Directories a glob matches are skipped.
func (run *unitRun) uploadOutputs(runContext context.Context) string {
	if len(run.unit.Outputs) == 0 {
		return protocol.StatusPassed
	}
	status := protocol.StatusPassed
	workspace, err := filepath.EvalSymlinks(run.workspace)
	if err != nil {
		run.fail(protocol.PhaseUpload, err)
		return protocol.StatusBroken
	}
	uploaded := map[string]bool{}
	for _, output := range run.unit.Outputs {
		matches, err := fs.Glob(os.DirFS(workspace), output.Glob)
		if err != nil {
			run.fail(protocol.PhaseUpload, fmt.Errorf("output glob %q: %w", output.Glob, err))
			status = worse(status, protocol.StatusFailed)
			continue
		}
		files := 0
		for _, match := range matches {
			resolved, err := filepath.EvalSymlinks(filepath.Join(workspace, filepath.FromSlash(match)))
			if err != nil {
				run.fail(protocol.PhaseUpload, fmt.Errorf("output %s: %w", match, err))
				status = worse(status, protocol.StatusFailed)
				continue
			}
			if !inside(workspace, resolved) {
				run.fail(protocol.PhaseUpload, fmt.Errorf("refused: output %s resolves to %s, outside the workspace", match, resolved))
				status = worse(status, protocol.StatusFailed)
				continue
			}
			info, err := os.Stat(resolved)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			files++
			if uploaded[match] {
				continue
			}
			uploaded[match] = true
			hash, size, err := hashFile(resolved)
			if err == nil {
				err = run.uploadBlob(runContext, hash, resolved, size)
			}
			if err != nil {
				run.fail(protocol.PhaseUpload, fmt.Errorf("output %s: %w", match, err))
				status = worse(status, protocol.StatusBroken)
				continue
			}
			run.emitter.emit(protocol.Event{Type: "uploaded", Path: match, Sha256: hash, Bytes: size})
		}
		if files == 0 {
			run.fail(protocol.PhaseUpload, fmt.Errorf("output glob %q matched no file", output.Glob))
			status = worse(status, protocol.StatusFailed)
		}
	}
	return status
}

func hashFile(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hasher := sha256.New()
	size, err := io.Copy(hasher, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hasher.Sum(nil)), size, nil
}

// uploadBlob PUTs a file to <store>/<sha256>, retrying a failure the store may get over.
func (run *unitRun) uploadBlob(runContext context.Context, hash string, path string, size int64) error {
	var lastError error
	for attempt := range storeAttempts {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
		}
		retry, err := run.put(runContext, hash, path, size)
		if err == nil {
			return nil
		}
		lastError = err
		if !retry || runContext.Err() != nil {
			break
		}
	}
	return lastError
}

func (run *unitRun) put(runContext context.Context, hash string, path string, size int64) (retry bool, err error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	request, err := http.NewRequestWithContext(runContext, http.MethodPut, storeUrl(run.unit.Store.Url, hash), file)
	if err != nil {
		return false, err
	}
	request.ContentLength = size
	request.Header.Set("Content-Type", "application/octet-stream")
	if run.unit.Token != "" {
		request.Header.Set("Authorization", "Bearer "+run.unit.Token)
	}
	response, err := run.options.Client.Do(request)
	if err != nil {
		return true, err
	}
	io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	response.Body.Close()
	if response.StatusCode/100 != 2 {
		retry := response.StatusCode/100 == 5 || response.StatusCode == http.StatusTooManyRequests
		return retry, fmt.Errorf("PUT %s: %s", request.URL.Redacted(), response.Status)
	}
	return false, nil
}
