package cmd

import "syscall"

// detachedProcess is DETACHED_PROCESS: the daemon gets no console.
const detachedProcess = 0x00000008

// detached starts the daemon without a console and in a process group of
// its own: closing the console vartop ran in (or its Ctrl+C) does not reach
// it.
func detached() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: detachedProcess | syscall.CREATE_NEW_PROCESS_GROUP}
}
