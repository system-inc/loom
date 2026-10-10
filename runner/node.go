package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/system-inc/loom/builder"
)

// A phase job runs on a checkout, which holds no npm packages, and a runner never installs them (#v03v751): the job
// names its future's tree, and the runner places each npm project's packages from that tree's build, the chunk of its
// source Workshop installed them into (builder/node.go), at <root>/adamic-npm/<lockfile sha256>/node_modules, where
// prepare.sh links them into the checkout when its lockfile is that one. Placed once per lockfile and kept, as the npm
// trees prepare.sh made were, so a warm runner fetches and unpacks nothing; the chunk comes through the blob cache,
// its hash checked, and unpacks as builder.UnpackChunk does.

// nodeDirectory is where a runner keeps npm packages by lockfile, prepare.sh's ${root}/adamic-npm.
const nodeDirectory = "adamic-npm"

// placeNodePackages places every npm project the tree's build names that the root doesn't hold yet.
func (run *unitRun) placeNodePackages(placeContext context.Context, treeKey, root string) error {
	index, err := run.treeIndex(placeContext, treeKey)
	if err != nil {
		return err
	}
	if len(index.Node) == 0 {
		run.say("the tree's build names no npm packages")
		return nil
	}
	if err = readyRoot(run.options, root); err != nil {
		return fmt.Errorf("refused as unfit: %w", err)
	}
	cache := newBlobCache(run.options, root)
	chunks := map[string]builder.SourceChunk{}
	for _, chunk := range index.Source {
		chunks[chunk.Blob] = chunk
	}
	for _, project := range index.Node {
		placed := filepath.Join(root, nodeDirectory, project.Lockfile, "node_modules")
		if _, err := os.Stat(placed); err == nil {
			run.say(fmt.Sprintf("%s's npm packages, lockfile %s, are already here", project.Directory, project.Lockfile))
			continue
		}
		started := time.Now()
		file, fetch, err := cache.open(placeContext, project.Chunk)
		if errors.Is(err, builder.ErrNotStored) {
			err = fmt.Errorf("blob %s isn't in the store (never uploaded, or past its 7 days)", project.Chunk)
		}
		if err != nil {
			return fmt.Errorf("%s's npm packages: %w", project.Directory, err)
		}
		run.fetched("chunks", fetch.cached, fetch.bytes)
		err = placeChunk(placeContext, file, chunks[project.Chunk], project, filepath.Join(root, nodeDirectory))
		file.Close()
		if err != nil {
			return fmt.Errorf("%s's npm packages: %w", project.Directory, err)
		}
		run.say(fmt.Sprintf("placed %s's npm packages, lockfile %s, chunk %s, %d bytes, in %.1f s", project.Directory, project.Lockfile, project.Chunk,
			fetch.bytes, time.Since(started).Seconds()))
	}
	return nil
}

// placeChunk unpacks a project's chunk in staging under directory and moves its node_modules to
// <directory>/<lockfile>/node_modules whole, so a runner killed partway leaves only staging, which prepare.sh's trim
// takes (adamic-npm/*.staging-*). Packages another unit placed meanwhile are kept as they are.
func placeChunk(placeContext context.Context, file *os.File, chunk builder.SourceChunk, project builder.NodeProject, directory string) error {
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(directory, project.Lockfile+".staging-")
	if err != nil {
		return err
	}
	defer removeDirectory(staging)
	if err = builder.UnpackChunk(contextReader{placeContext, file}, filepath.Join(staging, "tree"), chunk); err != nil {
		return err
	}
	if err = os.Rename(filepath.Join(staging, "tree", filepath.FromSlash(project.Directory), "node_modules"), filepath.Join(staging, "node_modules")); err != nil {
		return err
	}
	// Only the directories the chunk's names went through are left.
	if err = removeDirectory(filepath.Join(staging, "tree")); err != nil {
		return err
	}
	final := filepath.Join(directory, project.Lockfile)
	if err = os.Rename(staging, final); err != nil {
		if _, held := os.Stat(filepath.Join(final, "node_modules")); held == nil {
			return nil
		}
		return err
	}
	return nil
}
