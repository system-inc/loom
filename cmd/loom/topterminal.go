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
// stops it. stop puts every setting back.
type topTerminal struct {
	output *os.File
	input  *os.File
	saved  *syscall.Termios
}

type windowSize struct {
	rows, columns, xPixels, yPixels uint16
}

// openTerminal is the terminal stdout is, when it is one.
func openTerminal(stdout io.Writer) (*topTerminal, bool) {
	file, isFile := stdout.(*os.File)
	if !isFile || ioctlGetTermios == 0 {
		return nil, false
	}
	terminal := &topTerminal{output: file, input: os.Stdin}
	if columns, rows := terminal.size(); columns == 0 || rows == 0 {
		return nil, false
	}
	return terminal, true
}

// size is the terminal's columns and rows, 80 by 24 when it can't say.
func (terminal *topTerminal) size() (int, int) {
	var size windowSize
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, terminal.output.Fd(), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&size))); errno != 0 || size.columns == 0 {
		return 80, 24
	}
	return int(size.columns), int(size.rows)
}

// start takes the screen (the alternate one, the cursor hidden, as btop does) and the keyboard, and gives each key
// pressed. Without a terminal on stdin no key ever comes, and ^C still stops it.
func (terminal *topTerminal) start() <-chan byte {
	fmt.Fprint(terminal.output, "\033[?1049h\033[?25l\033[2J")
	keys := make(chan byte, 8)
	var settings syscall.Termios
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, terminal.input.Fd(), ioctlGetTermios, uintptr(unsafe.Pointer(&settings))); errno != 0 {
		return keys
	}
	saved := settings
	terminal.saved = &saved
	settings.Lflag &^= syscall.ICANON | syscall.ECHO
	settings.Cc[syscall.VMIN], settings.Cc[syscall.VTIME] = 1, 0
	syscall.Syscall(syscall.SYS_IOCTL, terminal.input.Fd(), ioctlSetTermios, uintptr(unsafe.Pointer(&settings)))
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
	return keys
}

// stop gives the terminal back as it was.
func (terminal *topTerminal) stop() {
	if terminal.saved != nil {
		syscall.Syscall(syscall.SYS_IOCTL, terminal.input.Fd(), ioctlSetTermios, uintptr(unsafe.Pointer(terminal.saved)))
	}
	fmt.Fprint(terminal.output, "\033[?25h\033[?1049l")
}
