package runner

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/system-inc/loom/builder"
)

// fetchProducts places each product the unit runs, by its key, from the action store (contracts v1.1). The store's
// format has one reader, Builder's: builder.Store.Fetch reads refs/action/<key>, its canonical manifest and every blob,
// checking each hash, size and path, into a scratch directory of the runner's own. Only then is each file copied into
// the workspace through an os.Root, so nothing an earlier input left there (a symlink, say) can steer a product
// outside it, and nothing is placed over a file already there. The store is public and read without the run's token.
func (run *unitRun) fetchProducts(runContext context.Context) error {
	if len(run.unit.Products) == 0 {
		return nil
	}
	workspace, err := os.OpenRoot(run.workspace)
	if err != nil {
		return err
	}
	defer workspace.Close()
	store := builder.Store{Read: strings.TrimSuffix(run.unit.ProductStore, "/"), Client: run.options.Client}
	for index, product := range run.unit.Products {
		if runContext.Err() != nil {
			return runContext.Err()
		}
		fetched := filepath.Join(run.staging, fmt.Sprintf("product-%d", index))
		if err := store.Fetch(product.Key, fetched); err != nil {
			return fmt.Errorf("product %s: %w", product.Key, err)
		}
		err := filepath.WalkDir(fetched, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			relative, err := filepath.Rel(fetched, path)
			if err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			mode := "644"
			if info.Mode().Perm()&0o111 != 0 {
				mode = "755"
			}
			return placeFile(workspace, filepath.Join(filepath.FromSlash(product.Directory), relative), mode, path)
		})
		os.RemoveAll(fetched)
		if err != nil {
			return fmt.Errorf("product %s: %w", product.Key, err)
		}
	}
	return nil
}
