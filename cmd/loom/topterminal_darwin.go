package main

import "syscall"

// The ioctls that read and set a terminal's settings.
const (
	ioctlGetTermios uintptr = syscall.TIOCGETA
	ioctlSetTermios uintptr = syscall.TIOCSETA
)
