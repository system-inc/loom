package resident

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/system-inc/loom/planner"
)

// A tracked file is one path of a commit's tree as git holds it: its mode, its object, and the repository holding the
// object ("" for the tree's own, else the submodule's path in the tree).
type tracked struct {
	Mode       string
	Object     string
	Repository string
}

// symlink says whether git holds the path as a symbolic link, whose content as go reads it is its target's.
func (file tracked) symlink() bool {
	return file.Mode == "120000"
}

// listTracked lists every file a checked-out commit tracks, submodules recursed through the commits their gitlinks
// record, each repository asked in its own checkout: the tree's paths to what git holds for each, and each gitlink's
// path to its commit. Nothing is read from the files themselves; a checkout's submodule that doesn't hold its
// gitlink's commit fails, named.
func listTracked(checkout, commit string) (map[string]tracked, map[string]string, error) {
	files, gitlinks := map[string]tracked{}, map[string]string{}
	var walk func(repository, revision string) error
	walk = func(repository, revision string) error {
		output, err := planner.LocalGit(filepath.Join(checkout, filepath.FromSlash(repository)), "ls-tree", "-r", "-z", "--full-tree", revision).Output()
		if err != nil {
			return fmt.Errorf("git ls-tree %s in %q: %w", revision, repository, gitError(err))
		}
		for _, entry := range strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00") {
			if entry == "" {
				continue
			}
			// <mode> SP <type> SP <object> TAB <path>
			head, name, found := strings.Cut(entry, "\t")
			fields := strings.Fields(head)
			if !found || len(fields) != 3 {
				return fmt.Errorf("git ls-tree in %q: an entry %q", repository, entry)
			}
			full := name
			if repository != "" {
				full = path.Join(repository, name)
			}
			switch fields[1] {
			case "commit":
				gitlinks[full] = fields[2]
				if err := walk(full, fields[2]); err != nil {
					return err
				}
			case "blob":
				files[full] = tracked{Mode: fields[0], Object: fields[2], Repository: repository}
			}
		}
		return nil
	}
	if err := walk("", commit); err != nil {
		return nil, nil, err
	}
	return files, gitlinks, nil
}

// gitError is err with git's own words, when it said any.
func gitError(err error) error {
	var exit *exec.ExitError
	if errors.As(err, &exit) && len(exit.Stderr) > 0 {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exit.Stderr)))
	}
	return err
}

// readObjects reads each object of one repository through one `git cat-file --batch` and gives each its sha256: what a
// checkout of it holds, byte for byte, with no file of the checkout read.
func readObjects(checkout, repository string, objects []string) (map[string]string, error) {
	command := planner.LocalGit(filepath.Join(checkout, filepath.FromSlash(repository)), "cat-file", "--batch")
	var input bytes.Buffer
	for _, object := range objects {
		input.WriteString(object + "\n")
	}
	command.Stdin = &input
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		return nil, err
	}
	sums := map[string]string{}
	reader := bufio.NewReaderSize(output, 1<<20)
	readErr := func() error {
		for range objects {
			header, err := reader.ReadString('\n')
			if err != nil {
				return fmt.Errorf("git cat-file --batch: %w", err)
			}
			// <object> SP <type> SP <size> LF <content> LF
			fields := strings.Fields(header)
			if len(fields) != 3 || fields[1] != "blob" {
				return fmt.Errorf("git cat-file --batch in %q: %q", repository, strings.TrimSpace(header))
			}
			size, err := strconv.ParseInt(fields[2], 10, 64)
			if err != nil {
				return err
			}
			hash := sha256.New()
			if _, err := io.CopyN(hash, reader, size); err != nil {
				return err
			}
			if _, err := reader.ReadByte(); err != nil {
				return err
			}
			sums[fields[0]] = hex.EncodeToString(hash.Sum(nil))
		}
		return nil
	}()
	io.Copy(io.Discard, reader)
	if err := command.Wait(); err != nil {
		return nil, fmt.Errorf("git cat-file --batch in %q: %w: %s", repository, err, strings.TrimSpace(stderr.String()))
	}
	return sums, readErr
}
