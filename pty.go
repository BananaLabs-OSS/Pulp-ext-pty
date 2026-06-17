// Package ptyext provides the spawn.pty capability for Pulp cells: real
// interactive pseudo-terminals. The cell cannot spawn a PTY (it is sandboxed
// WASM), so the host does it here — ConPTY on Windows, a unix PTY on Linux/Mac,
// both via github.com/aymanbagabas/go-pty. The cell opens a PTY, forwards
// keystrokes (pty_write) and resizes (pty_resize), and the host streams the
// shell's output back as `pty.output` step events (Poll). The cell bridges
// those to the browser's xterm.js WebSocket.
//
// Shell choice is per-OS + per-request: cmd / powershell / pwsh on Windows,
// bash / sh on Linux. This is how the cockpit's terminal pane runs CMD,
// PowerShell, and Linux shells from one portable cell.
//
// Deployment:
//
//	import _ "github.com/BananaLabs-OSS/Pulp-ext-pty"
//
// Host imports:
//
//	pty_open(req_ptr, req_len, resp_ptr_out, resp_len_out) -> code   # req{shell}; resp{pty_id}
//	pty_write(pty_id, data_ptr, data_len) -> code
//	pty_resize(pty_id, cols, rows) -> code
//	pty_close(pty_id) -> code
package ptyext

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"

	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/aymanbagabas/go-pty"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/vmihailenco/msgpack/v5"
)

const (
	codeOK            = 0
	codeBadReq        = 1
	codeMemRead       = 2
	codeDecode        = 3
	codeSpawnFailed   = 4
	codeNoSession     = 6
	codeAllocFailed   = 7
	codeMemWrite      = 8
	codeCapAbsent     = 99
	maxBufferPerPTY   = 1 << 20 // 1 MiB of un-drained output per PTY, then oldest dropped
	readChunk         = 4096
)

type session struct {
	id     uint32
	cellID string
	pty    pty.Pty
	cmd    *pty.Cmd

	mu     sync.Mutex
	out    []byte // buffered output awaiting Poll
	closed bool
}

var (
	mu       sync.Mutex
	sessions = map[uint32]*session{}
	order    []uint32 // round-robin Poll fairness
	nextID   uint32
	nextEvID uint64
	logger   = slog.Default()
)

func init() {
	ext.Register(ext.Capability{
		Name:         "spawn.pty",
		Setup:        setup,
		Teardown:     teardown,
		TeardownCell: teardownCell,
		Register:     bindActive,
		Stub:         bindStub,
		Poll:         pollOutput,
		// Finalize is a no-op: output is drained into the event at Poll time.
	})
}

func setup(env ext.SetupEnv) error {
	if env.Logger != nil {
		logger = env.Logger
	}
	logger.Info("spawn.pty ready", "os", runtime.GOOS)
	return nil
}

func teardown(_ context.Context) error {
	mu.Lock()
	defer mu.Unlock()
	for id, s := range sessions {
		if s != nil && s.cmd != nil {
			killPtyTree(s.cmd.Process)
		}
		_ = s.pty.Close()
		delete(sessions, id)
	}
	order = nil
	return nil
}

// teardownCell closes ONLY the named cell's PTY sessions (its shell processes) on
// `ctl reload`. Without this, a self-rebuild leaves the old cell's shells + their
// reader goroutines alive for the host's lifetime — they accumulate across rebuilds.
func teardownCell(_ context.Context, cellID string) error {
	mu.Lock()
	defer mu.Unlock()
	for id, s := range sessions {
		if s != nil && s.cellID == cellID {
			if s.cmd != nil {
				killPtyTree(s.cmd.Process)
			}
			_ = s.pty.Close()
			delete(sessions, id)
		}
	}
	kept := order[:0] // prune order to sessions that still exist
	for _, id := range order {
		if _, ok := sessions[id]; ok {
			kept = append(kept, id)
		}
	}
	order = kept
	return nil
}

// ---- shell selection -------------------------------------------------------

// commandLineForShell maps a requested shell to a command line for this OS.
// Windows: cmd / powershell / pwsh. Unix: bash / sh (or $SHELL). Unknown =>
// the OS default interactive shell.
func commandLineForShell(shell string) []string {
	if shell == "agent" {
		return agentCommandLine()
	}
	if runtime.GOOS == "windows" {
		switch shell {
		case "cmd":
			if p, err := exec.LookPath("cmd.exe"); err == nil {
				return []string{p}
			}
			return []string{"cmd.exe"}
		case "pwsh":
			if p, err := exec.LookPath("pwsh.exe"); err == nil {
				return []string{p, "-NoLogo"}
			}
			fallthrough
		case "powershell", "":
			if p, err := exec.LookPath("powershell.exe"); err == nil {
				return []string{p, "-NoLogo"}
			}
			return []string{"powershell.exe", "-NoLogo"}
		default:
			return []string{"powershell.exe", "-NoLogo"}
		}
	}
	// unix
	switch shell {
	case "bash":
		if p, err := exec.LookPath("bash"); err == nil {
			return []string{p}
		}
	case "sh", "":
		if sh := os.Getenv("SHELL"); sh != "" {
			return []string{sh}
		}
	default:
		if p, err := exec.LookPath(shell); err == nil {
			return []string{p}
		}
	}
	if p, err := exec.LookPath("bash"); err == nil {
		return []string{p}
	}
	return []string{"/bin/sh"}
}

// agentCommandLine resolves the coding-agent CLI (env PROJX_AGENT_CMD, default
// "claude") for the "agent" pane. On Windows a .cmd/.bat shim can't be launched
// by CreateProcess directly, so it's wrapped through cmd.exe /c. Not found =>
// return the bare name so it fails visibly in the pane.
func agentCommandLine() []string {
	cmd := os.Getenv("PROJX_AGENT_CMD")
	if cmd == "" {
		cmd = "claude"
	}
	abs, err := exec.LookPath(cmd)
	if err != nil {
		return []string{cmd}
	}
	if runtime.GOOS == "windows" {
		low := strings.ToLower(abs)
		if strings.HasSuffix(low, ".cmd") || strings.HasSuffix(low, ".bat") {
			return []string{"cmd.exe", "/c", abs}
		}
	}
	return []string{abs}
}

// ---- binding ---------------------------------------------------------------

func bindActive(b wazero.HostModuleBuilder, cell ext.Cell) error {
	cellID := ""
	if cell != nil {
		cellID = cell.Name()
	}
	b.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module, reqPtr, reqLen, respPtrOut, respLenOut uint32) uint32 {
		return ptyOpen(ctx, m, cellID, reqPtr, reqLen, respPtrOut, respLenOut)
	}).Export("pty_open")
	b.NewFunctionBuilder().WithFunc(func(_ context.Context, m api.Module, id, dataPtr, dataLen uint32) uint32 {
		return ptyWrite(m, id, dataPtr, dataLen)
	}).Export("pty_write")
	b.NewFunctionBuilder().WithFunc(func(_ context.Context, _ api.Module, id, cols, rows uint32) uint32 {
		return ptyResize(id, cols, rows)
	}).Export("pty_resize")
	b.NewFunctionBuilder().WithFunc(func(_ context.Context, _ api.Module, id uint32) uint32 {
		return ptyClose(id)
	}).Export("pty_close")
	return nil
}

func bindStub(b wazero.HostModuleBuilder, _ ext.Cell) error {
	b.NewFunctionBuilder().WithFunc(func(_ context.Context, _ api.Module, _, _, _, _ uint32) uint32 { return codeCapAbsent }).Export("pty_open")
	b.NewFunctionBuilder().WithFunc(func(_ context.Context, _ api.Module, _, _, _ uint32) uint32 { return codeCapAbsent }).Export("pty_write")
	b.NewFunctionBuilder().WithFunc(func(_ context.Context, _ api.Module, _, _, _ uint32) uint32 { return codeCapAbsent }).Export("pty_resize")
	b.NewFunctionBuilder().WithFunc(func(_ context.Context, _ api.Module, _ uint32) uint32 { return codeCapAbsent }).Export("pty_close")
	return nil
}

// ---- handlers --------------------------------------------------------------

func ptyOpen(ctx context.Context, m api.Module, cellID string, reqPtr, reqLen, respPtrOut, respLenOut uint32) uint32 {
	var req struct {
		Shell string   `msgpack:"shell"`
		Args  []string `msgpack:"args"`
		Dir   string   `msgpack:"dir"`
	}
	if reqLen > 0 {
		data, ok := m.Memory().Read(reqPtr, reqLen)
		if !ok {
			return codeMemRead
		}
		if err := msgpack.Unmarshal(data, &req); err != nil {
			return codeDecode
		}
	}
	line := append(commandLineForShell(req.Shell), req.Args...)
	p, err := pty.New()
	if err != nil {
		logger.Error("pty new", "err", err)
		return codeSpawnFailed
	}
	cmd := p.Command(line[0], line[1:]...)
	if req.Dir != "" {
		cmd.Dir = req.Dir
	}
	if err := cmd.Start(); err != nil {
		_ = p.Close()
		logger.Error("pty start", "shell", req.Shell, "err", err)
		return codeSpawnFailed
	}
	superviseProcess(cmd.Process) // enroll the shell so its whole tree dies with us (no orphans)

	mu.Lock()
	nextID++
	id := nextID
	s := &session{id: id, cellID: cellID, pty: p, cmd: cmd}
	sessions[id] = s
	order = append(order, id)
	mu.Unlock()

	go readLoop(s)

	resp, _ := msgpack.Marshal(struct {
		PtyID uint32 `msgpack:"pty_id"`
	}{PtyID: id})
	return writeResp(ctx, m, resp, respPtrOut, respLenOut)
}

// readLoop pumps PTY output into the session buffer until EOF/close.
func readLoop(s *session) {
	buf := make([]byte, readChunk)
	for {
		n, err := s.pty.Read(buf)
		if n > 0 {
			s.mu.Lock()
			s.out = append(s.out, buf[:n]...)
			if len(s.out) > maxBufferPerPTY {
				s.out = s.out[len(s.out)-maxBufferPerPTY:]
			}
			s.mu.Unlock()
		}
		if err != nil {
			s.mu.Lock()
			s.closed = true
			s.mu.Unlock()
			return
		}
	}
}

func ptyWrite(m api.Module, id, dataPtr, dataLen uint32) uint32 {
	s := getSession(id)
	if s == nil {
		return codeNoSession
	}
	data, ok := m.Memory().Read(dataPtr, dataLen)
	if !ok {
		return codeMemRead
	}
	if _, err := s.pty.Write(data); err != nil {
		return codeSpawnFailed
	}
	return codeOK
}

func ptyResize(id, cols, rows uint32) uint32 {
	s := getSession(id)
	if s == nil {
		return codeNoSession
	}
	if err := s.pty.Resize(int(cols), int(rows)); err != nil {
		return codeSpawnFailed
	}
	return codeOK
}

func ptyClose(id uint32) uint32 {
	mu.Lock()
	s := sessions[id]
	delete(sessions, id)
	for i, x := range order {
		if x == id {
			order = append(order[:i], order[i+1:]...)
			break
		}
	}
	mu.Unlock()
	if s == nil {
		return codeNoSession
	}
	if s.cmd != nil {
		killPtyTree(s.cmd.Process) // kill the shell + its children, not just close the device
	}
	_ = s.pty.Close()
	return codeOK
}

func getSession(id uint32) *session {
	mu.Lock()
	defer mu.Unlock()
	return sessions[id]
}

// pollOutput drains one session's buffered output per call (round-robin) and
// emits it as a pty.output event for that session's cell. Returns ok=false when
// nothing is pending.
func pollOutput() (ext.StepEvent, bool) {
	mu.Lock()
	ids := append([]uint32(nil), order...)
	mu.Unlock()
	for _, id := range ids {
		s := getSession(id)
		if s == nil {
			continue
		}
		s.mu.Lock()
		if len(s.out) == 0 {
			s.mu.Unlock()
			continue
		}
		data := s.out
		s.out = nil
		s.mu.Unlock()

		payload, err := msgpack.Marshal(struct {
			PtyID uint32 `msgpack:"pty_id"`
			Data  []byte `msgpack:"data"`
		}{PtyID: id, Data: data})
		if err != nil {
			continue
		}
		mu.Lock()
		nextEvID++
		evID := nextEvID
		// rotate this id to the back for fairness
		for i, x := range order {
			if x == id {
				order = append(append(order[:i:i], order[i+1:]...), id)
				break
			}
		}
		mu.Unlock()
		return ext.StepEvent{Kind: "pty.output", Payload: payload, ID: evID, CellID: s.cellID}, true
	}
	return ext.StepEvent{}, false
}

func writeResp(ctx context.Context, m api.Module, data []byte, respPtrOut, respLenOut uint32) uint32 {
	allocFn := m.ExportedFunction("pulp_alloc")
	if allocFn == nil {
		return codeAllocFailed
	}
	res, err := allocFn.Call(ctx, uint64(len(data)))
	if err != nil || len(res) == 0 {
		return codeAllocFailed
	}
	ptr := uint32(res[0])
	if ptr == 0 || !m.Memory().Write(ptr, data) {
		return codeMemWrite
	}
	if !m.Memory().WriteUint32Le(respPtrOut, ptr) || !m.Memory().WriteUint32Le(respLenOut, uint32(len(data))) {
		return codeMemWrite
	}
	return codeOK
}
