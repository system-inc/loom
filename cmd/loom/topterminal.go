package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

// lineEnds clears each row to its end before the next, so a shorter row leaves nothing of the last frame's.
var lineEnds = strings.NewReplacer("\n", "\033[K\n")

// A topTerminal is the terminal `loom top` draws on, through the standard library alone: its size by TIOCGWINSZ, and
// its input put in non-canonical mode without echo, so q arrives as it is pressed, with signals left on, so ^C still
// stops it. suspend and stop put every setting back; resume takes them again after ^Z and fg.
type topTerminal struct {
	output *os.File
	input  *os.File
	// saved are the input's settings as found, raw the ones `loom top` reads keys with; nil when stdin isn't a terminal.
	saved, raw *syscall.Termios
	// columns and rows are the size last read, kept when a read fails.
	columns, rows int
}

type windowSize struct {
	rows, columns, xPixels, yPixels uint16
}

func ioctl(file *os.File, request uintptr, argument unsafe.Pointer) bool {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), request, uintptr(argument))
	return errno == 0
}

// openTerminal is the terminal stdout is, when it is one: a file whose size and settings the terminal ioctls read. A
// file, a pipe or ssh without a terminal is none, and `loom top` draws one frame there and exits.
func openTerminal(stdout io.Writer) (*topTerminal, bool) {
	file, isFile := stdout.(*os.File)
	if !isFile || ioctlGetTermios == 0 {
		return nil, false
	}
	var settings syscall.Termios
	if !ioctl(file, ioctlGetTermios, unsafe.Pointer(&settings)) {
		return nil, false
	}
	terminal := &topTerminal{output: file, input: os.Stdin, columns: 80, rows: 24}
	if !terminal.measure() {
		return nil, false
	}
	return terminal, true
}

// measure reads the terminal's columns and rows, and says whether the ioctl answered. A terminal that answers 0 by 0
// (a pty nobody sized) keeps the size it had, 80 by 24 at first.
func (terminal *topTerminal) measure() bool {
	var size windowSize
	if !ioctl(terminal.output, uintptr(syscall.TIOCGWINSZ), unsafe.Pointer(&size)) {
		return false
	}
	if size.columns > 0 && size.rows > 0 {
		terminal.columns, terminal.rows = int(size.columns), int(size.rows)
	}
	return true
}

// size is the terminal's columns and rows: as read now, or as last read when it can't say.
func (terminal *topTerminal) size() (int, int) {
	terminal.measure()
	return terminal.columns, terminal.rows
}

// start takes the screen and the keyboard and gives each key pressed. Without a terminal on stdin no key ever comes,
// and ^C still stops it.
func (terminal *topTerminal) start() <-chan byte {
	keys := make(chan byte, 8)
	var settings syscall.Termios
	if ioctl(terminal.input, ioctlGetTermios, unsafe.Pointer(&settings)) {
		saved, raw := settings, settings
		raw.Lflag &^= syscall.ICANON | syscall.ECHO
		raw.Cc[syscall.VMIN], raw.Cc[syscall.VTIME] = 1, 0
		terminal.saved, terminal.raw = &saved, &raw
		go func() {
			buffer := make([]byte, 16)
			for {
				count, err := terminal.input.Read(buffer)
				if err != nil {
					close(keys)
					return
				}
				for _, key := range buffer[:count] {
					keys <- key
				}
			}
		}()
	}
	terminal.resume()
	return keys
}

// resume takes the screen (the alternate one, the cursor hidden, as btop does) and puts the input in raw mode: at the
// start, and again when a shell's fg continues `loom top`, whose settings the shell put back when it stopped.
func (terminal *topTerminal) resume() {
	if terminal.raw != nil {
		ioctl(terminal.input, ioctlSetTermios, unsafe.Pointer(terminal.raw))
	}
	fmt.Fprint(terminal.output, "\033[?1049h\033[?25l\033[2J")
}

// suspend gives the terminal back as it was found: before ^Z stops `loom top`, and at the end.
func (terminal *topTerminal) suspend() {
	if terminal.saved != nil {
		ioctl(terminal.input, ioctlSetTermios, unsafe.Pointer(terminal.saved))
	}
	fmt.Fprint(terminal.output, "\033[?25h\033[?1049l")
}

// stop gives the terminal back for good.
func (terminal *topTerminal) stop() {
	terminal.suspend()
}
