//go:build !windows

package cmd

import "syscall"

// detached starts the daemon in a session of its own: neither the
// terminal vartop ran in closing (SIGHUP) nor its job control reach it.
func detached() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
