//go:build !windows

package ipc

import "syscall"

// detachAttr starts the daemon in a new session so it outlives the client
// process that spawned it.
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
