package planner

import (
	"bufio"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// TraceCalls is the strace call list a traced run is recorded with, for a read set and its check:
//
//	strace -f --decode-fds=path -e trace=<TraceCalls> -o <trace> <the unit's test binary> ...
//
// so a directory descriptor prints as 5</abs/dir> and AT_FDCWD as AT_FDCWD</abs/cwd>. The forks, chdir and fchdir are
// there so a call that names a relative path without a directory descriptor (open, stat, execve) resolves against its
// own process's working directory; the calls that create, move and remove paths so what the run made itself is never
// its input. A trace of only open, openat, openat2 and execve serves the declared-reads check, never a read set: it
// can't show a run that stat'ed, missed or listed a path.
const TraceCalls = "open,openat,openat2,execve,execveat,stat,lstat,newfstatat,fstatat64,statx,access,faccessat,faccessat2," +
	"readlink,readlinkat,getdents,getdents64,chdir,fchdir,clone,clone3,fork,vfork," +
	"mkdir,mkdirat,unlink,unlinkat,rmdir,rename,renameat,renameat2,link,linkat,symlink,symlinkat"

// traceCall is one strace-format syscall line: an optional pid ("123 " or "[pid 123] "), the call, its arguments and
// its result.
var traceCall = regexp.MustCompile(`^(?:\[pid\s+(\d+)\]\s+|(\d+)\s+)?([a-z0-9_]+)\((.*)\)\s+=\s+(-?\d+)`)

// unfinished and resumed are strace's split form when two processes' calls interleave.
var (
	unfinished = regexp.MustCompile(`^(?:\[pid\s+(\d+)\]\s+|(\d+)\s+)?(.*)\s<unfinished \.\.\.>$`)
	resumed    = regexp.MustCompile(`^(?:\[pid\s+(\d+)\]\s+|(\d+)\s+)?<\.\.\. (\w+) resumed>(.*)$`)
)

// The calls a trace names a path with: the first argument a path, or a directory descriptor and then a path.
var (
	pathCalls      = map[string]bool{"open": true, "execve": true, "stat": true, "lstat": true, "access": true, "readlink": true, "chdir": true}
	directoryCalls = map[string]bool{"openat": true, "openat2": true, "newfstatat": true, "fstatat64": true, "statx": true,
		"faccessat": true, "faccessat2": true, "readlinkat": true, "execveat": true}
	listingCalls = map[string]bool{"getdents": true, "getdents64": true}
	forkCalls    = map[string]bool{"clone": true, "clone3": true, "fork": true, "vfork": true}
)

// TracedAccesses is every path a trace shows a run reaching, absolute. Reads are what it opened for reading or
// executed and got; Lookups every other path it named, stat'ed, probed or missed (a failed open of any kind but a
// write); Listings every directory it listed. Present is the lookups that found the path there before the run made
// anything of it: with the reads and the listings, what the run saw exist. Made is what the run made itself (a
// directory's whole contents with it), which a directory's listing on disk afterward holds and the tree never did.
type TracedAccesses struct {
	Reads    []string
	Lookups  []string
	Listings []string
	Present  []string
	Made     []string
}

// TracedReads is every file a trace shows read: a successful open, openat or openat2 that isn't write-only, or a
// successful execve. Each is an absolute path; a relative one resolves against its decoded directory descriptor, or
// its process's working directory (directory for the traced process, until it moves).
func TracedReads(trace io.Reader, directory string) ([]string, error) {
	accesses, err := TraceAccesses(trace, directory)
	return accesses.Reads, err
}

// A traceEvent is one finished call of one process, in the order the trace finished it, except a fork, which is in
// the order it started: its child is born with the working directory its parent had then.
type traceEvent struct {
	pid, call, arguments, line string
	result                     int
}

// TraceAccesses reads a trace into the paths its run reached (TracedAccesses). A path resolves as TracedReads says.
// Each process's working directory is the decoded AT_FDCWD of its latest *at call, or what its chdir or fchdir made
// it, or its parent's when it forked; threads and processes cloned with CLONE_FS share one, so any of them moves
// it. The traced process starts in directory. A listing names its descriptor's decoded path, and one the trace didn't
// decode is refused, since its directory is unknown; so is a call by a numbered descriptor the trace didn't decode.
func TraceAccesses(trace io.Reader, directory string) (TracedAccesses, error) {
	events, err := traceEvents(trace)
	if err != nil {
		return TracedAccesses{}, err
	}
	reads, lookups, listings, present := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	working := map[string]string{}
	// shared is each thread or process that shares its creator's working directory (CLONE_FS, which every thread
	// has), to the first of them: a chdir by any one moves them all.
	shared := map[string]string{}
	group := func(pid string) string {
		if first, found := shared[pid]; found {
			return first
		}
		return pid
	}
	workingOf := func(pid string) string {
		if cwd, found := working[group(pid)]; found {
			return cwd
		}
		return directory
	}
	// made is every path whose state the run itself set, from the call that set it on: a file it created or truncated,
	// a directory it made (and so everything under it), a rename's or a link's target, and what it renamed away or
	// removed. A later access to one reads the run's own doing, never the tree, so it isn't the run's input. An access
	// before that call still is: a miss before a create keys that the path was absent.
	made := map[string]bool{}
	madeByRun := func(path string) bool {
		for candidate := path; ; candidate = filepath.Dir(candidate) {
			if made[candidate] {
				return true
			}
			if parent := filepath.Dir(candidate); parent == candidate {
				return false
			}
		}
	}
	lookup := func(path string, found bool) {
		if !madeByRun(path) {
			lookups[path] = true
			if found {
				present[path] = true
			}
		}
	}
	for _, event := range events {
		if match := descriptor.FindStringSubmatch(event.arguments); match != nil && strings.HasPrefix(match[0], "AT_FDCWD<") {
			// strace decodes AT_FDCWD as the process's working directory as it is: the truth, whatever came before.
			working[group(event.pid)] = filepath.Clean(match[1])
		}
		failed := func(err error) (TracedAccesses, error) {
			return TracedAccesses{}, fmt.Errorf("trace line %q: %w", event.line, err)
		}
		switch {
		case forkCalls[event.call]:
			if event.result > 0 {
				child := strconv.Itoa(event.result)
				if strings.Contains(event.arguments, "CLONE_FS") || strings.Contains(event.arguments, "CLONE_THREAD") {
					shared[child] = group(event.pid)
				} else {
					working[child] = workingOf(event.pid)
				}
			}
		case event.call == "fchdir":
			if event.result == 0 {
				match := descriptor.FindStringSubmatch(event.arguments + ", ")
				if match == nil || match[1] == "" {
					return failed(fmt.Errorf("an fchdir whose directory wasn't decoded (--decode-fds=path)"))
				}
				working[group(event.pid)] = filepath.Clean(match[1])
			}
		case listingCalls[event.call]:
			if event.result < 0 {
				continue
			}
			match := descriptor.FindStringSubmatch(event.arguments + ", ")
			if match == nil || match[1] == "" {
				return failed(fmt.Errorf("a listing whose directory wasn't decoded (--decode-fds=path)"))
			}
			if listed := filepath.Clean(match[1]); !madeByRun(listed) {
				listings[listed] = true
			}
		case changeCalls[event.call] != nil:
			change := changeCalls[event.call]
			arguments := event.arguments
			if change.target {
				// symlink and symlinkat name the link's target first: text the link holds, never a path read.
				_, rest, err := quoted(arguments)
				if err != nil {
					return failed(err)
				}
				arguments = nextArgument(rest)
			}
			first, rest, _, err := pathArgument(arguments, workingOf(event.pid), change.descriptors)
			if err != nil {
				return failed(err)
			}
			if !change.two {
				if !change.target {
					// A removal that worked found the path; a mkdir found it only when it was already there.
					found := event.result == 0
					if strings.HasPrefix(event.call, "mkdir") {
						found = strings.Contains(event.line, "EEXIST")
					}
					lookup(first, found)
				}
				if event.result == 0 {
					made[first] = true
				}
				continue
			}
			second, _, _, err := pathArgument(nextArgument(rest), workingOf(event.pid), change.descriptors)
			if err != nil {
				return failed(err)
			}
			lookup(first, event.result == 0)
			if event.result == 0 {
				if change.moves {
					made[first] = true
				}
				made[second] = true
			}
		case pathCalls[event.call] || directoryCalls[event.call]:
			path, flags, dotted, err := pathArgument(event.arguments, workingOf(event.pid), directoryCalls[event.call])
			if err != nil {
				return failed(err)
			}
			opened := event.call == "open" || event.call == "openat" || event.call == "openat2"
			executed := event.call == "execve" || event.call == "execveat"
			decoded := ""
			if match := openedPath.FindStringSubmatch(event.line); match != nil {
				decoded = filepath.Clean(match[1])
			}
			switch {
			case event.call == "chdir":
				lookup(path, event.result == 0)
				if event.result == 0 {
					working[group(event.pid)] = path
				}
			case opened && event.result >= 0 && (strings.Contains(flags, "O_TRUNC") || strings.Contains(flags, "O_CREAT") && strings.Contains(flags, "O_EXCL")):
				// A file the run created or emptied holds only what the run writes into it.
				made[path] = true
				if decoded != "" {
					made[decoded] = true
				}
			case opened && strings.Contains(flags, "O_WRONLY"):
				// A write is never a read; one that opened a file says it was there (or that the run made it).
				if event.result >= 0 {
					lookup(path, false)
				}
			case (opened || executed) && event.result >= 0 && !strings.Contains(flags, "O_PATH"):
				// The descriptor's decoded path is the file the kernel opened, every symlink resolved: that read is
				// the run's too, wherever the name it opened by led, a link the run made itself included. A name with a
				// .. in it is cleaned as text, which past a symlink names another path, so the decoded one stands alone.
				if !madeByRun(path) && !(dotted && decoded != "") {
					reads[path] = true
				}
				if decoded != "" && !madeByRun(decoded) {
					reads[decoded] = true
				}
			default:
				lookup(path, event.result >= 0)
			}
		}
	}
	return TracedAccesses{Reads: sortedKeys(reads), Lookups: sortedKeys(lookups), Listings: sortedKeys(listings), Present: sortedKeys(present),
		Made: sortedKeys(made)}, nil
}

// A pathChange is a call that changes the tree's paths: whether its paths follow directory descriptors, whether it
// names two (a source and a target), whether it moves the source away, and whether it starts with a symlink's target.
type pathChange struct {
	descriptors, two, moves, target bool
}

// changeCalls are the calls that create, move or remove a path. Each is in TraceCalls.
var changeCalls = map[string]*pathChange{
	"mkdir": {}, "mkdirat": {descriptors: true}, "unlink": {}, "unlinkat": {descriptors: true}, "rmdir": {},
	"rename": {two: true, moves: true}, "renameat": {descriptors: true, two: true, moves: true},
	"renameat2": {descriptors: true, two: true, moves: true}, "link": {two: true}, "linkat": {descriptors: true, two: true},
	"symlink": {target: true}, "symlinkat": {descriptors: true, target: true},
}

// nextArgument is what follows an argument's separating comma.
func nextArgument(rest string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), ","))
}

// traceEvents reads a trace's finished calls in order, joining each call strace split around another process's.
func traceEvents(trace io.Reader) ([]traceEvent, error) {
	events := []traceEvent{}
	type started struct {
		text string
		fork int // the fork's event, placed where it started; -1 for any other call
	}
	pending := map[string]started{}
	scanner := bufio.NewScanner(trace)
	scanner.Buffer(make([]byte, 1<<20), 1<<24)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if match := unfinished.FindStringSubmatch(line); match != nil {
			pid, text := match[1]+match[2], match[3]
			start := started{text: text, fork: -1}
			if name, _, found := strings.Cut(text, "("); found && forkCalls[name] {
				start.fork = len(events)
				events = append(events, traceEvent{pid: pid, call: name, line: line})
			}
			pending[pid] = start
			continue
		}
		fork := -1
		if match := resumed.FindStringSubmatch(line); match != nil {
			start, found := pending[match[1]+match[2]]
			if !found {
				continue
			}
			delete(pending, match[1]+match[2])
			line = strings.TrimSpace(match[1] + match[2] + " " + start.text + match[4])
			fork = start.fork
		}
		match := traceCall.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		result, err := strconv.Atoi(match[5])
		if err != nil {
			continue
		}
		event := traceEvent{pid: match[1] + match[2], call: match[3], arguments: match[4], line: line, result: result}
		if fork >= 0 {
			events[fork] = event
			continue
		}
		events = append(events, event)
	}
	return events, scanner.Err()
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// openedPath is a successful open's decoded result, = 3</resolved/path>.
var openedPath = regexp.MustCompile(`\)\s+=\s+\d+<(.+)>$`)

// descriptor is a decoded directory argument: AT_FDCWD</dir> or 5</dir>.
var descriptor = regexp.MustCompile(`^(?:AT_FDCWD|\d+)(?:<(.*?)>)?,\s*`)

// pathArgument reads a call's next path argument, absolute, what follows it, and whether the name held a .. after a
// name. With descriptor, a directory descriptor comes first and a relative path resolves against it, decoded, or
// against the process's working directory for AT_FDCWD; without, against the working directory. An empty path after
// a descriptor (AT_EMPTY_PATH) is the descriptor's own. The path is cleaned as text, so a .. after a symlink may name
// another path than the kernel reached, which resolves .. from where the link led.
func pathArgument(arguments, working string, withDescriptor bool) (string, string, bool, error) {
	base := working
	if withDescriptor {
		match := descriptor.FindStringSubmatch(arguments)
		if match == nil {
			return "", "", false, fmt.Errorf("no directory argument")
		}
		if match[1] != "" {
			base = match[1]
		} else if !strings.HasPrefix(match[0], "AT_FDCWD") {
			return "", "", false, fmt.Errorf("a directory descriptor that wasn't decoded (--decode-fds=path)")
		}
		arguments = arguments[len(match[0]):]
	}
	name, rest, err := quoted(arguments)
	if err != nil {
		return "", "", false, err
	}
	// A leading .. climbs from the base, which strace decodes as the kernel's own path; a .. after a name in the path
	// climbs from wherever that name led, which cleaning as text can't know.
	dotted, named := false, false
	for _, component := range strings.Split(name, "/") {
		switch component {
		case "", ".":
		case "..":
			dotted = dotted || named
		default:
			named = true
		}
	}
	if name == "" {
		name = base
	}
	if dotted {
		return uncleanedPath(base, name), rest, dotted, nil
	}
	if !filepath.IsAbs(name) {
		name = filepath.Join(base, name)
	}
	return filepath.Clean(name), rest, dotted, nil
}

// uncleanedPath is a name with a .. after a name, absolute, as the kernel will walk it: a leading .. climbs from the
// base (strace decodes the kernel's own path), and every .. after a name stays, for the index to resolve from wherever
// that name led (submoduleIndex.resolve).
func uncleanedPath(base, name string) string {
	components := []string{}
	if !filepath.IsAbs(name) {
		for _, component := range strings.Split(filepath.ToSlash(filepath.Clean(base)), "/") {
			if component != "" {
				components = append(components, component)
			}
		}
	}
	named := false
	for _, component := range strings.Split(name, "/") {
		switch {
		case component == "" || component == ".":
		case component == ".." && !named:
			if len(components) > 0 {
				components = components[:len(components)-1]
			}
		default:
			named = named || component != ".."
			components = append(components, component)
		}
	}
	return "/" + strings.Join(components, "/")
}

// quoted reads strace's leading C string argument and returns it with what follows.
func quoted(arguments string) (string, string, error) {
	if !strings.HasPrefix(arguments, `"`) {
		return "", "", fmt.Errorf("no path argument")
	}
	for index := 1; index < len(arguments); index++ {
		switch arguments[index] {
		case '\\':
			index++
		case '"':
			name, err := strconv.Unquote(arguments[:index+1])
			if err != nil {
				return "", "", fmt.Errorf("path %s: %w", arguments[:index+1], err)
			}
			return name, arguments[index+1:], nil
		}
	}
	return "", "", fmt.Errorf("unterminated path")
}
