package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// prepare.sh pin-urls holds every submodule url at every depth to pinUrl's rule (queuebridge's PinUrl, the same rule):
// a checkout whose nested submodule names a house address is refused by name, though the top level is on GitHub.
// Mutants: pinUrls reading only the top level's .gitmodules, or splitting the submodule paths on spaces, make this fail.
func TestPrepareChecksEverySubmoduleUrlAtEveryDepth(t *testing.T) {
	// The real path, as git names it (macOS's /tmp is /private/tmp).
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	environment := append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_ALLOW_PROTOCOL=file")
	git := func(where string, arguments ...string) {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", where, "-c", "protocol.file.allow=always"}, arguments...)...)
		command.Env = environment
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(arguments, " "), err, output)
		}
	}
	repository := func(name string, files map[string]string) string {
		directory := filepath.Join(root, name)
		git(root, "init", "-q", "-b", "main", directory)
		for file, text := range files {
			os.WriteFile(filepath.Join(directory, file), []byte(text), 0o644)
		}
		git(directory, "add", "-A")
		git(directory, "commit", "-q", "--allow-empty", "-m", name)
		return directory
	}
	// The house address is named only two levels down, in cohere's own .gitmodules.
	typeScript := repository("TypeScript", map[string]string{"a.ts": "let a = 1\n"})
	cohere := repository("cohere", nil)
	git(cohere, "submodule", "add", "-q", typeScript, "TypeScript")
	git(cohere, "config", "-f", ".gitmodules", "submodule.TypeScript.url", "http://10.101.1.1/TypeScript.git")
	git(cohere, "commit", "-q", "-am", "pins TypeScript")
	// cohere sits at a path with a space, so a check that splits paths on spaces never reads its .gitmodules.
	tree := repository("adamic", nil)
	git(tree, "submodule", "add", "-q", cohere, "co here")
	// cohere's TypeScript is checked out from the local repository; its .gitmodules still names the house address.
	nested := filepath.Join(tree, "co here")
	git(nested, "submodule", "init")
	git(nested, "config", "submodule.TypeScript.url", typeScript)
	git(nested, "submodule", "update", "-q")
	git(tree, "config", "-f", ".gitmodules", "submodule.co here.url", "git@github.com:system-inc/cohere.git")
	git(tree, "commit", "-q", "-am", "pins cohere")
	check := func() (int, string) {
		command := exec.Command("bash", "prepare.sh", "pin-urls", tree)
		command.Env = environment
		output, _ := command.CombinedOutput()
		return command.ProcessState.ExitCode(), string(output)
	}
	if code, output := check(); code != 3 || !strings.Contains(output, "refused: submodule http://10.101.1.1/TypeScript.git in "+nested) {
		t.Fatalf("a house address two levels down: exit %d, %s", code, output)
	}
	git(nested, "config", "-f", ".gitmodules", "submodule.TypeScript.url", "https://github.com/system-inc/TypeScript.git")
	if code, output := check(); code != 0 {
		t.Fatalf("every url on github.com: exit %d, %s", code, output)
	}
}
