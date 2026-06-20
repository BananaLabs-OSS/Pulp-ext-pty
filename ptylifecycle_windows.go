//go:build windows

package ptyext

import (
	"os"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ptyChildAttr returns the SysProcAttr for the PTY shell. It MUST NOT set
// CREATE_NO_WINDOW: go-pty starts the shell attached to a ConPTY pseudoconsole
// (conhost runs --headless, so there is no window to suppress), and ORing
// CREATE_NO_WINDOW into the ConPTY CreateProcess flags detaches the child from
// the pseudoconsole — the shell's stdout then never reaches the ConPTY read
// side, so readLoop gets zero bytes and the terminal/agent pane stays blank.
// (go-pty ignores HideWindow; only CreationFlags is honored — so an empty attr
// is correct, and the pseudoconsole already guarantees no console window.)
func ptyChildAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}

// PTY shells (cmd / powershell / pwsh) spawn their OWN children — a `git push`, an
// `ssh`, an agent. Closing the ConPTY device does NOT kill them, so without this they
// orphan and lock files (the ".runtime can't be deleted" bug). Each shell goes in its
// own Job Object with KILL_ON_JOB_CLOSE: an explicit close terminates the whole tree,
// and host exit (even a bare os.Exit / crash) closes the handle so the OS reaps it.

var (
	ptyJobsMu sync.Mutex
	ptyJobs   = map[int]windows.Handle{}
)

func superviseProcess(proc *os.Process) {
	if proc == nil {
		return
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(proc.Pid))
	if err != nil {
		windows.CloseHandle(job)
		return
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		windows.CloseHandle(job)
		return
	}
	ptyJobsMu.Lock()
	ptyJobs[proc.Pid] = job
	ptyJobsMu.Unlock()
}

// killPtyTree terminates the shell and every descendant via its Job Object. Called on
// pty close / teardown / cell reload.
func killPtyTree(proc *os.Process) {
	if proc == nil {
		return
	}
	ptyJobsMu.Lock()
	job, ok := ptyJobs[proc.Pid]
	if ok {
		delete(ptyJobs, proc.Pid)
	}
	ptyJobsMu.Unlock()
	if ok {
		_ = windows.TerminateJobObject(job, 1)
		windows.CloseHandle(job)
		return
	}
	_ = proc.Kill()
}
