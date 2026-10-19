//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package main

import "golang.org/x/sys/unix"

func isTerminal(fd uintptr) bool {
	_, err := unix.IoctlGetTermios(int(fd), unix.TIOCGETA)
	return err == nil
}
