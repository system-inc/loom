package resident

import (
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/system-inc/loom/builder"
	"github.com/system-inc/loom/planner"
)

// A ColdRead is a checked-out tree keyed the way Workshop keys one without a resident: builder.TestPackages, then
// planner.Closure for each package, one go list and every closure file read from disk per package.
type ColdRead struct {
	Packages []planner.ProductTest
	Closures map[string]Closure
	Seconds  float64
}

// Cold keys the tree checked out at checkout without a resident.
func Cold(checkout string) (ColdRead, error) {
	started := time.Now()
	packages, err := builder.TestPackages(checkout)
	if err != nil {
		return ColdRead{}, err
	}
	read := ColdRead{Packages: packages, Closures: map[string]Closure{}}
	for _, test := range packages {
		key, err := planner.Closure(checkout, test.Package)
		if err != nil {
			read.Closures[test.Package] = Closure{Error: err.Error()}
			continue
		}
		read.Closures[test.Package] = Closure{Key: key}
	}
	read.Seconds = time.Since(started).Seconds()
	return read, nil
}

// Check compares the resident's tree with a cold read of the same checkout and lists every difference: a test package
// one lists and the other doesn't, a closure key that differs, a closure one keys and the other refuses, and a closure
// file whose sha256 from the resident's memo isn't the sha256 of the file's own bytes on disk. None is the rule kept.
// It says how many closure files it read from disk, so a check that compared nothing can't pass for one that did.
func Check(checkout string, resident *Resident, tree *Tree, cold ColdRead) ([]string, int) {
	differences := []string{}
	warm := map[string]string{}
	for _, test := range tree.Packages {
		warm[test.Package] = test.Directory
	}
	for _, test := range cold.Packages {
		if directory, held := warm[test.Package]; !held {
			differences = append(differences, fmt.Sprintf("package %s: cold lists it, the resident doesn't", test.Package))
		} else if directory != test.Directory {
			differences = append(differences, fmt.Sprintf("package %s: cold has it in %s, the resident in %s", test.Package, test.Directory, directory))
		}
		delete(warm, test.Package)
	}
	for name := range warm {
		differences = append(differences, fmt.Sprintf("package %s: the resident lists it, cold doesn't", name))
	}
	for name, coldClosure := range cold.Closures {
		warmClosure, held := tree.Closures[name]
		switch {
		case !held:
		case (coldClosure.Error == "") != (warmClosure.Error == ""):
			differences = append(differences, fmt.Sprintf("package %s: cold's closure is %q (error %q), the resident's %q (error %q)", name, coldClosure.Key, coldClosure.Error, warmClosure.Key, warmClosure.Error))
		case coldClosure.Key != warmClosure.Key:
			differences = append(differences, fmt.Sprintf("package %s: cold keys its closure %s, the resident %s", name, coldClosure.Key, warmClosure.Key))
		}
	}
	resident.mutex.Lock()
	defer resident.mutex.Unlock()
	checked := map[string]bool{}
	for name, closure := range tree.Closures {
		for _, file := range closure.files {
			if checked[file] {
				continue
			}
			checked[file] = true
			memo, err := resident.sum(checkout, tree, file)
			if err != nil {
				differences = append(differences, fmt.Sprintf("package %s: file %s: %v", name, file, err))
				continue
			}
			disk := file
			if !filepath.IsAbs(file) {
				disk = filepath.Join(checkout, filepath.FromSlash(file))
			}
			actual, err := diskSum(disk)
			if err != nil {
				differences = append(differences, fmt.Sprintf("file %s: %v", file, err))
			} else if actual != memo {
				differences = append(differences, fmt.Sprintf("file %s: the resident holds sha256 %s, the file on disk is %s", file, memo, actual))
			}
		}
	}
	sort.Strings(differences)
	return differences, len(checked)
}

// Stale makes the resident a mutant: every later advance keeps the warm tree's object for path, a stale hash, which
// Check must catch. For `loom resident-check --mutant` and the tests alone.
func (resident *Resident) Stale(path string) {
	resident.mutex.Lock()
	defer resident.mutex.Unlock()
	resident.stale = path
}
