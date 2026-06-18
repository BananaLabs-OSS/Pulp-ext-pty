//go:build !windows

package ptyext

import (
	"os"
	"syscall"
)

// ptyChildAttr is nil off Windows — let go-pty set its own (Setsid/Setctty) defaults.
func ptyChildAttr() *syscall.SysProcAttr { return nil }

// superviseProcess is a no-op off Windows: go-pty starts the shell with Setsid, so it's
// already a session/process-group leader — killPtyTree can signal the whole group.
func superviseProcess(proc *os.Process) {}

// killPtyTree SIGKILLs the shell's entire process group (negative pid), so its children
// (git, ssh, an agent's grandchildren) die too — not just the shell.
func killPtyTree(proc *os.Process) {
	if proc == nil {
		return
	}
	if err := syscall.Kill(-proc.Pid, syscall.SIGKILL); err != nil {
		_ = proc.Kill()
	}
}
