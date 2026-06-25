//go:build linux

package ptyext

import (
	"os"
	"syscall"
)

// ptyChildAttr sets Pdeathsig on Linux so that if the host process dies (crash /
// os.Exit) the kernel automatically SIGKILLs every PTY shell, preventing orphaned
// shells from holding file locks or running indefinitely.
//
// go-pty will add Setsid=true and Setctty=true on top of this attr before Start,
// so the full set in practice is: Setsid=true, Setctty=true, Pdeathsig=SIGKILL.
//
// (On macOS/BSD there is no Pdeathsig; those platforms keep the nil attr from
// ptylifecycle_other.go and rely on graceful teardown.)
func ptyChildAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}

// superviseProcess is a no-op on Linux: go-pty starts the shell with Setsid, so it's
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
