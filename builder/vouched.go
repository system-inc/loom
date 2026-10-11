package builder

import (
	"bytes"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/system-inc/loom/planner"
)

// checkSource held every file of a tree's source to its commit by hashing it again, every build: adamic's 98,382
// files took 1.04 s of a 3.1 s source step on Workshop (Oct 10, #s0cqqhk), though git already knows which of them
// can't have changed. Its index records each file's blob beside the file's stat data from when git last read it, and
// git diff-index against a commit, without --cached, lists every path whose index entry isn't the commit's blob and
// mode, or whose file's stat data isn't what the index recorded. A path it doesn't list is vouched for: checkSource
// checks its kind and executable bit from lstat as before, and takes its content as the commit's without reading it.
//
// The check still can't be skipped, and these are why it holds:
//
//   - A build step that writes a tracked file moves its ctime, which every write sets and nothing short of the
//     system's clock moves back, and its mtime and usually its size, so git lists it and checkSource hashes it as it
//     always did. A file written in the very second its index was (racy git) git can't trust by stat: it reads the
//     file itself, or smudged the entry when it wrote the index, so it no longer matches. A step that refreshes the
//     index after changing a file (go build's VCS stamping runs git status) records the changed file's blob, and that
//     isn't the commit's, so git lists it.
//   - An index can be told not to look. An entry marked assume-unchanged or skip-worktree, and one an fsmonitor
//     answered for, is trusted with no stat at all, so no entry with either bit is vouched for (ls-files -v tags
//     them), and every question here is asked with fsmonitor off and the settings that loosen the stat check
//     (core.checkStat minimal, core.trustctime false, core.ignoreStat, core.fileMode false) pinned to their defaults.
//   - Git compares a file's content after its conversions (a text attribute's line endings, a filter, ident, a
//     working-tree encoding, core.autocrlf), so a file it vouches for could hold other bytes than its blob, and the
//     source archives the bytes. So no path a conversion could apply to is vouched for (git check-attr): checkSource
//     hashes cohere's 10,584 files under text and eol=lf on adamic, as before.
//
// It isn't a defense against a forged index: one written by hand to match a changed file's stat data is believed, as
// git status believes it. The build runs only the tree's own code, and a source its manifest contradicts would fool
// only that tree's own tests, so the check guards against accidents, which leave stat data behind them. The attributes
// read are the checkout's as they are now (the .gitattributes files among them held to their commits here); one
// changed in .git/info/attributes or a global file between the checkout and the build is believed as it now reads.

// vouchingGit pins, for every question gitVouches asks, the settings that would let git trust a file without a stat
// or with less than all of it.
var vouchingGit = []string{"-c", "core.fsmonitor=false", "-c", "core.checkStat=default", "-c", "core.trustctime=true",
	"-c", "core.ignoreStat=false", "-c", "core.fileMode=true"}

// conversionAttributes are the attributes that decide whether git's content for a file is its bytes, and the only
// ones vouchedIn reads of what check-attr answers.
var conversionAttributes = []string{"text", "eol", "crlf", "filter", "ident", "working-tree-encoding"}

// gitVouches is the paths of the tree's source, submodules' under their paths, whose content git's index vouches is
// their commit's: each repository's entries with no assume-unchanged or skip-worktree bit, no conversion and no stat
// change, that diff-index against the commit the manifest describes doesn't list.
func gitVouches(tree string, repositories []trackedRepository) (map[string]bool, error) {
	vouched := make([][]string, len(repositories))
	err := each(len(repositories), len(repositories), func(index int) error {
		repository := repositories[index]
		paths, err := vouchedIn(filepath.Join(tree, filepath.FromSlash(repository.relative)), repository.commit)
		if err != nil {
			name := "the tree"
			if repository.relative != "" {
				name = "submodule " + repository.relative
			}
			return fmt.Errorf("asking %s's index what it vouches for: %w", name, err)
		}
		vouched[index] = paths
		for position, inner := range paths {
			vouched[index][position] = path.Join(repository.relative, inner)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	all := map[string]bool{}
	for _, paths := range vouched {
		for _, name := range paths {
			all[name] = true
		}
	}
	return all, nil
}

// vouchedIn is gitVouches for one repository, at directory, whose manifest describes commit: its paths, as git names
// them inside it. diff-index's stat of every file and core.autocrlf are asked while ls-files and check-attr read the
// index and the attributes, so the repository costs its slowest question rather than all four.
func vouchedIn(directory, commit string) ([]string, error) {
	var changed, autocrlf []byte
	var changedErr, autocrlfErr error
	var group sync.WaitGroup
	group.Go(func() {
		changed, changedErr = localGit(directory, append(slices.Clone(vouchingGit), "diff-index", "-z", "--name-only", "--no-renames", "--ignore-submodules=all", commit)...)
	})
	group.Go(func() {
		autocrlf, autocrlfErr = localGit(directory, "config", "--default", "false", "--get", "core.autocrlf")
	})
	defer group.Wait()
	listing, err := localGit(directory, append(slices.Clone(vouchingGit), "ls-files", "-z", "-v")...)
	if err != nil {
		return nil, err
	}
	candidates := []string{}
	for _, line := range splitNul(listing) {
		// "<tag> <path>": H is a cached entry git stats; h is assume-unchanged, S skip-worktree, M unmerged.
		if len(line) < 3 || line[1] != ' ' {
			return nil, fmt.Errorf("git ls-files -v lists %q", line)
		}
		if line[0] == 'H' {
			candidates = append(candidates, line[2:])
		}
	}
	answers := []byte{}
	if len(candidates) > 0 {
		// Every attribute a path has (none, for most), macros expanded: far less to read than six for every path.
		command := planner.LocalGit(directory, append(slices.Clone(vouchingGit), "check-attr", "-z", "--stdin", "--all")...)
		command.Stdin = strings.NewReader(strings.Join(candidates, "\x00") + "\x00")
		var stderr bytes.Buffer
		command.Stderr = &stderr
		if answers, err = command.Output(); err != nil {
			return nil, fmt.Errorf("git check-attr: %w: %s", err, strings.TrimSpace(stderr.String()))
		}
	}
	group.Wait()
	if changedErr != nil {
		return nil, changedErr
	}
	if autocrlfErr != nil {
		return nil, autocrlfErr
	}
	listed := map[string]bool{}
	for _, name := range splitNul(changed) {
		listed[name] = true
	}
	attributes := map[string]map[string]string{}
	fields := splitNul(answers)
	if len(fields)%3 != 0 {
		return nil, fmt.Errorf("git check-attr answered %d fields, not path, attribute and value triples", len(fields))
	}
	for index := 0; index < len(fields); index += 3 {
		if !slices.Contains(conversionAttributes, fields[index+1]) {
			continue
		}
		if attributes[fields[index]] == nil {
			attributes[fields[index]] = map[string]string{}
		}
		attributes[fields[index]][fields[index+1]] = fields[index+2]
	}
	converting := !slices.Contains([]string{"false", "no", "off", "0"}, strings.TrimSpace(string(autocrlf)))
	vouched := []string{}
	for _, name := range candidates {
		if !listed[name] && !converts(attributes[name], converting) {
			vouched = append(vouched, name)
		}
	}
	return vouched, nil
}

// converts reports whether git's content for a file with these attributes (by name; one not there is unspecified)
// could be other than its bytes: under a filter, ident or a working-tree encoding whatever it is, and as text unless
// text is unset (or, text unspecified, crlf is), which a text attribute, eol, crlf or, with none of them,
// core.autocrlf (autocrlf) makes it.
func converts(values map[string]string, autocrlf bool) bool {
	value := func(attribute string) string {
		if set, there := values[attribute]; there {
			return set
		}
		return "unspecified"
	}
	quiet := func(attribute string) bool {
		return value(attribute) == "unspecified" || value(attribute) == "unset"
	}
	if !quiet("filter") || !quiet("ident") || !quiet("working-tree-encoding") {
		return true
	}
	if value("text") == "unset" || (value("text") == "unspecified" && value("crlf") == "unset") {
		return false
	}
	return value("text") != "unspecified" || value("crlf") != "unspecified" || value("eol") != "unspecified" || autocrlf
}

// splitNul is git's -z output as its fields.
func splitNul(output []byte) []string {
	trimmed := strings.TrimSuffix(string(output), "\x00")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\x00")
}
