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
// own process's working directory. A trace of only open, openat, openat2 and execve serves the declared-reads check,
// never a read set: it can't show a run that stat'ed, missed or listed a path.
const TraceCalls = "open,openat,openat2,execve,execveat,stat,lstat,newfstatat,fstatat64,statx,access,faccessat,faccessat2," +
	"readlink,readlinkat,getdents,getdents64,chdir,fchdir,clone,clone3,fork,vfork"

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
// write); Listings every directory it listed.
type TracedAccesses struct {
	Reads    []string
	Lookups  []string
	Listings []string
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
// it, or its parent's when it forked; the traced process starts in directory. A listing names its descriptor's decoded
// path, and one the trace didn't decode is refused, since its directory is unknown; so is a call by a numbered
// descriptor the trace didn't decode.
func TraceAccesses(trace io.Reader, directory string) (TracedAccesses, error) {
	events, err := traceEvents(trace)
	if err != nil {
		return TracedAccesses{}, err
	}
	reads, lookups, listings := map[string]bool{}, map[string]bool{}, map[string]bool{}
	working := map[string]string{}
	workingOf := func(pid string) string {
		if cwd, found := working[pid]; found {
			return cwd
		}
		return directory
	}
	for _, event := range events {
		if match := descriptor.FindStringSubmatch(event.arguments); match != nil && strings.HasPrefix(match[0], "AT_FDCWD<") {
			// strace decodes AT_FDCWD as the process's working directory as it is: the truth, whatever came before.
			working[event.pid] = filepath.Clean(match[1])
		}
		switch {
		case forkCalls[event.call]:
			if event.result > 0 {
				working[strconv.Itoa(event.result)] = workingOf(event.pid)
			}
		case event.call == "fchdir":
			if event.result == 0 {
				match := descriptor.FindStringSubmatch(event.arguments + ", ")
				if match == nil || match[1] == "" {
					return TracedAccesses{}, fmt.Errorf("trace line %q: an fchdir whose directory wasn't decoded (--decode-fds=path)", event.line)
				}
				working[event.pid] = filepath.Clean(match[1])
			}
		case listingCalls[event.call]:
			if event.result < 0 {
				continue
			}
			match := descriptor.FindStringSubmatch(event.arguments + ", ")
			if match == nil || match[1] == "" {
				return TracedAccesses{}, fmt.Errorf("trace line %q: a listing whose directory wasn't decoded (--decode-fds=path)", event.line)
			}
			listings[filepath.Clean(match[1])] = true
		case pathCalls[event.call] || directoryCalls[event.call]:
			path, write, err := callPath(event.call, event.arguments, workingOf(event.pid))
			if err != nil {
				return TracedAccesses{}, fmt.Errorf("trace line %q: %w", event.line, err)
			}
			opened := event.call == "open" || event.call == "openat" || event.call == "openat2"
			executed := event.call == "execve" || event.call == "execveat"
			switch {
			case event.call == "chdir":
				lookups[path] = true
				if event.result == 0 {
					working[event.pid] = path
				}
			case write:
				// A write is never a read, and a failed one names nothing the run read either.
			case (opened || executed) && event.result >= 0 && !strings.Contains(event.arguments, "O_PATH"):
				reads[path] = true
				// The descriptor's decoded path is the file the kernel opened, every symlink resolved: that read is
				// the run's too, wherever the name it opened by led.
				if opened := openedPath.FindStringSubmatch(event.line); opened != nil {
					reads[filepath.Clean(opened[1])] = true
				}
			default:
				lookups[path] = true
			}
		}
	}
	return TracedAccesses{Reads: sortedKeys(reads), Lookups: sortedKeys(lookups), Listings: sortedKeys(listings)}, nil
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

// callPath is the absolute path a call named, and whether it was an open for writing only. A relative path resolves
// against the call's directory descriptor, decoded, or the process's working directory for AT_FDCWD and for a call
// that takes none. An empty path after a descriptor (AT_EMPTY_PATH) is the descriptor's own.
func callPath(call, arguments, working string) (string, bool, error) {
	base := working
	if directoryCalls[call] {
		match := descriptor.FindStringSubmatch(arguments)
		if match == nil {
			return "", false, fmt.Errorf("no directory argument")
		}
		if match[1] != "" {
			base = match[1]
		} else if !strings.HasPrefix(match[0], "AT_FDCWD") {
			return "", false, fmt.Errorf("a directory descriptor that wasn't decoded (--decode-fds=path)")
		}
		arguments = arguments[len(match[0]):]
	}
	name, rest, err := quoted(arguments)
	if err != nil {
		return "", false, err
	}
	write := (call == "open" || call == "openat" || call == "openat2") && strings.Contains(rest, "O_WRONLY")
	if name == "" {
		name = base
	}
	if !filepath.IsAbs(name) {
		name = filepath.Join(base, name)
	}
	return filepath.Clean(name), write, nil
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
