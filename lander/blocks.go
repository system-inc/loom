package lander

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
)

// The block builder (#7hn5em0), on Workshop beside the pusher, since the queue's Durable Object can't run git. For each
// block the queue opened and nobody built, it writes the block as a chain of merge commits on the branch's tip (tip, +A,
// +B, ...), each prefix a real commit, pushes the chain's tip as refs/loom/blocks/<n> (every prefix is on its
// first-parent line), and posts the chain to the queue: each prefix becomes the newest change's future, so halving a red
// block reuses work and landing a green prefix is a fast-forward to a tested commit. A change that conflicts with the
// changes ahead of it is left out and posted with the conflicting paths, which parks it and reaches its owner; the chain
// goes on without it.
//
// Nothing here runs with blocks off: the queue opens no block until a rule.changed turns them on.

// Identity is the chain's merge commits' author and committer.
var Identity = []string{"GIT_AUTHOR_NAME=kirkouimet", "GIT_AUTHOR_EMAIL=kirk@kirkouimet.com", "GIT_COMMITTER_NAME=kirkouimet", "GIT_COMMITTER_EMAIL=kirk@kirkouimet.com"}

// A BlockChange is one change of an unbuilt block as GET /blocks?state=unbuilt lists it.
type BlockChange struct {
	Change string `json:"change"`
	Sha    string `json:"sha"`
}

// A Prefix is one link of the chain: the merge commit and the change it adds.
type Prefix struct {
	Tree   string `json:"tree"`
	Change string `json:"change"`
}

// A Conflict is a change left out of the chain, with the paths it conflicts on.
type Conflict struct {
	Change string   `json:"change"`
	Paths  []string `json:"paths"`
}

// A Builder builds blocks: the branch's tip, a chain on a base, and the chain published.
type Builder interface {
	Tip() string
	Build(block int, base string, changes []BlockChange) (prefixes []Prefix, conflicts []Conflict, tip string, built bool)
	Publish(block int, tip string) bool
}

// Chain builds merge chains in a bare clone whose origin is GitHub (the lander's own, on Workshop), on Branch's tip.
type Chain struct {
	Repository string
	Branch     string
}

func (chain Chain) git(arguments ...string) (string, string, error) {
	return Git(chain.Repository, Identity, arguments...)
}

func (chain Chain) Tip() string {
	return Hands{Repository: chain.Repository, Branch: chain.Branch}.Tip()
}

// Build is the chain on base, or built false when git can't fetch or merge.
func (chain Chain) Build(block int, base string, changes []BlockChange) ([]Prefix, []Conflict, string, bool) {
	fetch := []string{"fetch", "-q", "--no-tags", "origin", base}
	for _, change := range changes {
		if !shaPattern.MatchString(change.Sha) {
			return nil, nil, "", false
		}
		fetch = append(fetch, change.Sha)
	}
	if _, _, err := chain.git(fetch...); err != nil {
		return nil, nil, "", false
	}
	prefixes, conflicts, prefix := []Prefix{}, []Conflict{}, base
	for _, change := range changes {
		stdout, _, err := chain.git("merge-tree", "--write-tree", "--name-only", "--no-messages", prefix, change.Sha)
		lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
		// merge-tree exits 1 for a conflict, its tree line first and then the conflicting paths.
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			paths := []string{}
			for _, line := range lines[1:] {
				if line != "" {
					paths = append(paths, line)
				}
			}
			slices.Sort(paths)
			conflicts = append(conflicts, Conflict{Change: change.Change, Paths: paths})
			continue
		}
		if err != nil || lines[0] == "" {
			return nil, nil, "", false
		}
		commit, _, err := chain.git("commit-tree", lines[0], "-p", prefix, "-p", change.Sha, "-m", fmt.Sprintf("Loom block %d: %s (%.12s)", block, change.Change, change.Sha))
		commit = strings.TrimSpace(commit)
		if err != nil || commit == "" {
			return nil, nil, "", false
		}
		prefixes = append(prefixes, Prefix{Tree: commit, Change: change.Change})
		prefix = commit
	}
	return prefixes, conflicts, prefix, true
}

func (chain Chain) Publish(block int, tip string) bool {
	_, _, err := chain.git("push", "-q", "origin", fmt.Sprintf("%s:refs/loom/blocks/%d", tip, block))
	return err == nil
}

// BuildBlocks is one pass over the unbuilt blocks: each built on the branch's tip, published, and posted.
func BuildBlocks(queue Queue, builder Builder, log func(string)) {
	status, answer := call(queue, "GET", "/blocks?state=unbuilt", nil)
	var listed struct {
		Blocks []struct {
			Block   int           `json:"block"`
			Changes []BlockChange `json:"changes"`
		} `json:"blocks"`
	}
	if status != 200 || json.Unmarshal(answer, &listed) != nil {
		log(fmt.Sprintf("blocks: %d %s", status, answerText(answer)))
		return
	}
	for _, block := range listed.Blocks {
		base := builder.Tip()
		prefixes, conflicts, tip, built := []Prefix(nil), []Conflict(nil), "", false
		if base != "" {
			prefixes, conflicts, tip, built = builder.Build(block.Block, base, block.Changes)
		}
		if !built {
			log(fmt.Sprintf("block %d: git couldn't build it; holding", block.Block))
			continue
		}
		if len(prefixes) > 0 && !builder.Publish(block.Block, tip) {
			log(fmt.Sprintf("block %d: pushing refs/loom/blocks/%d failed; holding", block.Block, block.Block))
			continue
		}
		status, answer := call(queue, "POST", fmt.Sprintf("/blocks/%d/built", block.Block), map[string]any{"base": base, "prefixes": prefixes, "conflicts": conflicts})
		log(fmt.Sprintf("block %d built on %.12s: %d prefixes, %d conflicts; %d %s", block.Block, base, len(prefixes), len(conflicts), status, answerText(answer)))
	}
}
