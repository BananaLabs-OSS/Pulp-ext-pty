package ptyext

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/ext"
)

func TestScopedPTYPolicyIsExactAndBounded(t *testing.T) {
	scope := ptyScope(t, "default")
	policy, root, shell := ptyPolicyFixture(t, scope)
	line, dir, env, err := policy.command("sh", nil, ".")
	if err != nil || line[0] != shell || dir != root || len(env) != 1 || env[0] != "TERM=xterm-256color" {
		t.Fatalf("command=%q dir=%q env=%q err=%v", line, dir, env, err)
	}
	for _, request := range []struct {
		shell string
		args  []string
		dir   string
	}{
		{"bash", nil, "."},
		{"sh", []string{"-c"}, "."},
		{"sh", nil, t.TempDir()},
	} {
		if _, _, _, err := policy.command(request.shell, request.args, request.dir); err == nil {
			t.Fatalf("accepted denied request %#v", request)
		}
	}
}

func TestScopedPTYPolicyRejectsCrossScopeAndMissingRight(t *testing.T) {
	scope := ptyScope(t, "default")
	_, root, shell := ptyPolicyFixture(t, scope)
	attrs := ptyGrantAttributes(t, root, shell)
	resolver, err := ext.NewStaticPlacementGrants([]ext.PlacementGrant{{Scope: scope, Capability: "spawn.pty", Resource: terminalResource, Rights: []string{"open"}, Attributes: attrs}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolveScopedPTYPolicy(resolver, ptyScope(t, "other")); err == nil {
		t.Fatal("grant crossed application instance")
	}
	resolver, _ = ext.NewStaticPlacementGrants([]ext.PlacementGrant{{Scope: scope, Capability: "spawn.pty", Resource: terminalResource, Rights: []string{"observe"}, Attributes: attrs}})
	if _, err := resolveScopedPTYPolicy(resolver, scope); err == nil {
		t.Fatal("missing open right accepted")
	}
}

func TestSessionHandlesDoNotCrossCellScope(t *testing.T) {
	mu.Lock()
	previous := sessions
	sessions = map[uint32]*session{7: {id: 7, cellID: "scope-a"}}
	mu.Unlock()
	t.Cleanup(func() { mu.Lock(); sessions = previous; mu.Unlock() })
	if getSessionForCell(7, "scope-a") == nil {
		t.Fatal("owner could not resolve session")
	}
	if getSessionForCell(7, "scope-b") != nil {
		t.Fatal("session handle crossed cell scope")
	}
	if ptyAlive("scope-b", 7) != 0 {
		t.Fatal("foreign cell observed session alive")
	}
}

func ptyScope(t *testing.T, instance string) ext.Scope {
	t.Helper()
	scope, err := ext.NewScope("projx", instance, "projx-terminal-session", "primary")
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func ptyPolicyFixture(t *testing.T, scope ext.Scope) (*scopedPTYPolicy, string, string) {
	t.Helper()
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip(err)
	}
	shell, _ = filepath.EvalSymlinks(shell)
	root := t.TempDir()
	resolver, err := ext.NewStaticPlacementGrants([]ext.PlacementGrant{{Scope: scope, Capability: "spawn.pty", Resource: terminalResource, Rights: []string{"open"}, Attributes: ptyGrantAttributes(t, root, shell)}})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := resolveScopedPTYPolicy(resolver, scope)
	if err != nil {
		t.Fatal(err)
	}
	return policy, root, shell
}

func ptyGrantAttributes(t *testing.T, root, shell string) map[string]string {
	t.Helper()
	executables, _ := json.Marshal(map[string]string{"sh": shell})
	environment, _ := json.Marshal(map[string]string{"TERM": "xterm-256color"})
	return map[string]string{"root": root, "executables_json": string(executables), "environment_json": string(environment), "max_argc": "0", "max_arg_bytes": "0"}
}
