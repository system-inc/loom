package builder

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/system-inc/loom/planner"
)

// productTestPattern finds a product test's declaration in a test file: adamic names every product test
// TestProduct_<name>, and run.py's first wave selects them the same way.
var productTestPattern = regexp.MustCompile(`(?m)^func (TestProduct_[A-Za-z0-9_]+)\(`)

type listedPackage struct {
	ImportPath   string
	Dir          string
	TestGoFiles  []string
	XTestGoFiles []string
}

// ListActions lists every product test on a tree, from go list's test files for this platform (so a file another
// platform's build tags leave out is left out here too), without compiling anything. Packages, when given, keeps only
// those import paths.
func ListActions(tree string, packages []string) ([]Action, error) {
	// The tree's own module, from its go.mod: in a workspace, go list -m names every module the workspace uses.
	goMod, err := os.ReadFile(filepath.Join(tree, "go.mod"))
	if err != nil {
		return nil, err
	}
	module := ""
	for _, line := range strings.Split(string(goMod), "\n") {
		if name, found := strings.CutPrefix(strings.TrimSpace(line), "module "); found {
			module = strings.Trim(strings.TrimSpace(name), `"`)
			break
		}
	}
	if module == "" {
		return nil, fmt.Errorf("%s/go.mod names no module", tree)
	}
	command := exec.Command("go", "list", "-json", "./...")
	command.Dir = tree
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("go list ./...: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	wanted := map[string]bool{}
	for _, importPath := range packages {
		wanted[importPath] = true
	}
	actions := []Action{}
	decoder := json.NewDecoder(bytes.NewReader(output))
	for decoder.More() {
		var listed listedPackage
		if err := decoder.Decode(&listed); err != nil {
			return nil, err
		}
		if len(wanted) > 0 && !wanted[listed.ImportPath] {
			continue
		}
		directory := strings.TrimPrefix(strings.TrimPrefix(listed.ImportPath, module), "/")
		seen := map[string]bool{}
		for _, file := range append(append([]string{}, listed.TestGoFiles...), listed.XTestGoFiles...) {
			content, err := os.ReadFile(filepath.Join(listed.Dir, file))
			if err != nil {
				return nil, err
			}
			for _, match := range productTestPattern.FindAllStringSubmatch(string(content), -1) {
				if !seen[match[1]] {
					seen[match[1]] = true
					actions = append(actions, Action{Package: listed.ImportPath, Directory: directory, Test: match[1]})
				}
			}
		}
	}
	sort.Slice(actions, func(left, right int) bool {
		if actions[left].Package != actions[right].Package {
			return actions[left].Package < actions[right].Package
		}
		return actions[left].Test < actions[right].Test
	})
	return actions, nil
}

// GateEnvironment is the environment a product test runs with on Workshop: the gate's switches, as a unit's, so
// what buildcache builds here under each key is what a gate unit would ask for.
func GateEnvironment() []string {
	environment := []string{}
	for name, value := range planner.GateEnvironment {
		environment = append(environment, name+"="+value)
	}
	sort.Strings(environment)
	return environment
}
