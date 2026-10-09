package runner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/system-inc/loom/protocol"
)

// maximumManifestBytes is the largest manifest the action store holds (wire/source/Actions.ts).
const maximumManifestBytes = 1 << 20

// An actionManifest is a product's outputs as the action store holds them: canonical JSON, keys sorted, outputs
// sorted by path, no builder and no time, so two honest builds of one key write the same bytes.
type actionManifest struct {
	Key     string         `json:"key"`
	Outputs []actionOutput `json:"outputs"`
}

type actionOutput struct {
	Bytes      int64  `json:"bytes"`
	Executable bool   `json:"executable"`
	Path       string `json:"path"`
	Sha256     string `json:"sha256"`
}

// fetchProducts places each product the unit runs, by its key, from the action store (contracts v1.1): the ref
// refs/action/<key> names a manifest blob, the manifest names its outputs, and each output is a blob. Every hash and
// size is checked, every path stays inside the product's directory, and nothing is placed over a file already there.
// The store is public and read without the run's token.
func (run *unitRun) fetchProducts(runContext context.Context) error {
	if len(run.unit.Products) == 0 {
		return nil
	}
	workspace, err := os.OpenRoot(run.workspace)
	if err != nil {
		return err
	}
	defer workspace.Close()
	store := strings.TrimSuffix(run.unit.ProductStore, "/")
	for _, product := range run.unit.Products {
		if err := run.fetchProduct(runContext, workspace, store, product); err != nil {
			return fmt.Errorf("product %s: %w", product.Key, err)
		}
	}
	return nil
}

func (run *unitRun) fetchProduct(runContext context.Context, workspace *os.Root, store string, product protocol.Product) error {
	ref, err := run.readRef(runContext, store+"/refs/action/"+product.Key)
	if err != nil {
		return err
	}
	manifestPath, err := run.fetchBlobFrom(runContext, store+"/blobs/"+ref, ref, false)
	if err != nil {
		return fmt.Errorf("manifest %s: %w", ref, err)
	}
	defer os.Remove(manifestPath)
	text, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	if len(text) > maximumManifestBytes {
		return fmt.Errorf("manifest %s is %d bytes, over %d", ref, len(text), maximumManifestBytes)
	}
	var manifest actionManifest
	if err := protocol.Decode(bytes.NewReader(text), &manifest); err != nil {
		return fmt.Errorf("manifest %s: %w", ref, err)
	}
	if manifest.Key != product.Key {
		return fmt.Errorf("manifest %s is for %s, not this product", ref, manifest.Key)
	}
	directory := filepath.FromSlash(product.Directory)
	seen := map[string]bool{}
	for _, output := range manifest.Outputs {
		path := filepath.FromSlash(output.Path)
		if output.Path == "" || !filepath.IsLocal(path) || seen[output.Path] {
			return fmt.Errorf("manifest %s: output path %q isn't a new path inside the product", ref, output.Path)
		}
		seen[output.Path] = true
		if !protocol.Sha256Pattern.MatchString(output.Sha256) {
			return fmt.Errorf("manifest %s: %s's sha256 isn't 64 lowercase hex digits", ref, output.Path)
		}
		blob, err := run.fetchBlobFrom(runContext, store+"/blobs/"+output.Sha256, output.Sha256, false)
		if err != nil {
			return fmt.Errorf("%s: %w", output.Path, err)
		}
		info, err := os.Stat(blob)
		if err == nil && info.Size() != output.Bytes {
			err = fmt.Errorf("refused: %d bytes, the manifest says %d", info.Size(), output.Bytes)
		}
		if err == nil {
			mode := "644"
			if output.Executable {
				mode = "755"
			}
			err = placeFile(workspace, filepath.Join(directory, path), mode, blob)
		}
		os.Remove(blob)
		if err != nil {
			return fmt.Errorf("%s: %w", output.Path, err)
		}
	}
	return nil
}

// readRef reads an action ref: the sha256 of its manifest blob, as plain text.
func (run *unitRun) readRef(runContext context.Context, url string) (string, error) {
	var lastError error
	for attempt := range storeAttempts {
		if attempt > 0 {
			sleepFor(runContext, attempt)
		}
		request, err := http.NewRequestWithContext(runContext, http.MethodGet, url, nil)
		if err != nil {
			return "", err
		}
		response, err := run.options.Client.Do(request)
		if err != nil {
			lastError = err
			continue
		}
		text, readError := io.ReadAll(io.LimitReader(response.Body, 1024))
		response.Body.Close()
		switch {
		case response.StatusCode == http.StatusOK && readError == nil:
			ref := strings.TrimSpace(string(text))
			if !protocol.Sha256Pattern.MatchString(ref) {
				return "", fmt.Errorf("GET %s: the ref holds %q, not a manifest's sha256", url, ref)
			}
			return ref, nil
		case response.StatusCode == http.StatusNotFound:
			return "", fmt.Errorf("GET %s: no such product in the store; it was never built", url)
		case response.StatusCode/100 == 5 || response.StatusCode == http.StatusTooManyRequests || readError != nil:
			lastError = fmt.Errorf("GET %s: %s", url, response.Status)
		default:
			return "", fmt.Errorf("GET %s: %s", url, response.Status)
		}
		if runContext.Err() != nil {
			break
		}
	}
	return "", lastError
}
