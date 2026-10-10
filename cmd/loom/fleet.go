package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/system-inc/loom/fleet"
	"github.com/system-inc/loom/protocol"
)

// fleetCommand is the fleet's verbs (#83911s7):
//
//	loom fleet                                          every source, its cap and its latest counts
//	loom fleet cap <source> <N>                         set a source's cap (0 sends no turns)
//	loom fleet add <source> --kind <k> --cap <N> [--tier <n>]
//	loom fleet off <source> | loom fleet on <source>
//	loom fleet counts <source> --running N --asking N ...   an arm's one write per pass
//	loom fleet cap-of <source> [--fallback <file>] [--default N]   the cap an arm honors now, for rearm.sh
//
// Every change names who made it (--by, default the user), and lands in Queue's log as a rule.changed event.
func fleetCommand(arguments []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("fleet", flag.ContinueOnError)
	flags.SetOutput(stderr)
	pipeline := flags.String("pipeline", "https://loom.system.inc", "loom's origin")
	by := flags.String("by", "", "who makes the change, for the log (default this user)")
	kind := flags.String("kind", "", "add: the source's kind ("+fmt.Sprint(fleet.Kinds)+")")
	capFlag := flags.Int("cap", -1, "add: the source's cap")
	tier := flags.Int("tier", -1, "add: the source's tier")
	fallback := flags.String("fallback", "", "cap-of: the file to read when loom doesn't answer (default ~/.loom/codex-ceiling)")
	def := flags.Int("default", fleet.DefaultCodexCap, "cap-of: the cap when neither answers")
	var counts fleet.Counts
	flags.IntVar(&counts.Running, "running", 0, "counts: members running a unit")
	flags.IntVar(&counts.Asking, "asking", 0, "counts: members asking for one")
	flags.IntVar(&counts.Warming, "warming", 0, "counts: members warming")
	flags.IntVar(&counts.Idle, "idle", 0, "counts: members idle")
	flags.IntVar(&counts.RefusedByReview, "refused-by-review", 0, "counts: members whose turn approval review refused")
	flags.IntVar(&counts.Retired, "retired", 0, "counts: members retired")
	flags.IntVar(&counts.TurnsLastHour, "turns-last-hour", 0, "counts: serve turns sent in the last hour")
	verb, rest := "", arguments
	if len(arguments) > 0 && arguments[0] != "" && arguments[0][0] != '-' {
		verb, rest = arguments[0], arguments[1:]
	}
	var positional []string
	for len(rest) > 0 {
		if err := flags.Parse(rest); err != nil {
			return 3
		}
		rest = flags.Args()
		if len(rest) > 0 {
			positional, rest = append(positional, rest[0]), rest[1:]
		}
	}
	home, _ := os.UserHomeDir()
	secret, err := protocol.ReadTokenSecret(filepath.Join(home, ".loom", "token-secret"))
	if err != nil {
		fmt.Fprintf(stderr, "loom: %v\n", err)
		return 3
	}
	client := fleet.NewClient(*pipeline, secret, "fleet")
	callContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if *by == "" {
		*by = "loom fleet"
		if current, err := user.Current(); err == nil {
			*by = current.Username
		}
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "loom fleet: %v\n", err)
		return 1
	}
	set := func(name string, change fleet.Change) int {
		change.By = *by
		source, err := client.Set(callContext, name, change)
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "%s: %s, cap %d, tier %d, on %t (seq %d by %s)\n", source.Name, source.Kind, source.Cap, source.Tier, source.On, source.Changed.Seq, source.Changed.By)
		return 0
	}
	switch {
	case verb == "" && len(positional) == 0:
		sources, err := client.List(callContext)
		if err != nil {
			return fail(err)
		}
		table := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(table, "source\tkind\tcap\ttier\ton\trunning\tasking\twarming\tidle\trefused\tretired\tturns/h\tcounts at\tchanged")
		for _, source := range sources {
			c := fleet.Counts{}
			if source.Counts != nil {
				c = *source.Counts
			}
			fmt.Fprintf(table, "%s\t%s\t%d\t%d\t%t\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%s\tseq %d by %s\n", source.Name, source.Kind, source.Cap, source.Tier, source.On,
				c.Running, c.Asking, c.Warming, c.Idle, c.RefusedByReview, c.Retired, c.TurnsLastHour, c.At, source.Changed.Seq, source.Changed.By)
		}
		table.Flush()
		return 0
	case verb == "cap" && len(positional) == 2:
		value, err := strconv.Atoi(positional[1])
		if err != nil {
			return fail(fmt.Errorf("cap %q isn't a whole number", positional[1]))
		}
		return set(positional[0], fleet.Change{Cap: &value})
	case verb == "add" && len(positional) == 1 && *kind != "" && *capFlag >= 0:
		change := fleet.Change{Kind: kind, Cap: capFlag}
		if *tier >= 0 {
			change.Tier = tier
		}
		return set(positional[0], change)
	case (verb == "off" || verb == "on") && len(positional) == 1:
		on := verb == "on"
		return set(positional[0], fleet.Change{On: &on})
	case verb == "counts" && len(positional) == 1:
		if err := client.PostCounts(callContext, positional[0], counts); err != nil {
			return fail(err)
		}
		return 0
	case verb == "cap-of" && len(positional) == 1:
		if *fallback == "" {
			*fallback = filepath.Join(home, ".loom", "codex-ceiling")
		}
		value, from := client.CapOf(callContext, positional[0], *fallback, *def)
		fmt.Fprintf(stdout, "%d\n", value)
		fmt.Fprintf(stderr, "loom fleet: %s's cap is %d, from %s\n", positional[0], value, from)
		return 0
	}
	fmt.Fprint(stderr, "usage: loom fleet [cap <source> <N> | add <source> --kind <k> --cap <N> [--tier <n>] | off <source> | on <source> | counts <source> --running N ... | cap-of <source>] [--pipeline <url>] [--by <who>]\n")
	return 3
}
