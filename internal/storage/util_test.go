//go:build linux

package storage

import "golang.org/x/sys/unix"

// unixClose is a tiny indirection so tests can close file descriptors without
// importing x/sys/unix at the top of every test file.
func unixClose(fd int) error { return unix.Close(fd) }
