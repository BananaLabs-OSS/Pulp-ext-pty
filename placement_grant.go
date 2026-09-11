package ptyext

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BananaLabs-OSS/Pulp/ext"
)

const terminalResource = "interactive-terminal"

type scopedPTYPolicy struct {
	root        string
	executables map[string]string
	environment []string
	maxArgc     int
	maxArgBytes int
}

func resolveScopedPTYPolicy(r ext.PlacementGrantResolver, scope ext.Scope) (*scopedPTYPolicy, error) {
	grant, ok := r.ResolvePlacementGrant(scope, "spawn.pty")
	if !ok {
		return nil, errors.New("spawn.pty: exact placement grant required")
	}
	if grant.Resource != terminalResource || !grant.Allows("open") {
		return nil, errors.New("spawn.pty: interactive-terminal open grant required")
	}
	root := strings.TrimSpace(grant.Attributes["root"])
	if !filepath.IsAbs(root) {
		return nil, errors.New("spawn.pty: scoped root must be absolute")
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil, errors.New("spawn.pty: scoped root must be a directory")
	}
	var configured map[string]string
	if err := json.Unmarshal([]byte(grant.Attributes["executables_json"]), &configured); err != nil || len(configured) == 0 {
		return nil, errors.New("spawn.pty: executables_json is required")
	}
	executables := make(map[string]string, len(configured))
	for name, raw := range configured {
		name, raw = strings.TrimSpace(strings.ToLower(name)), strings.TrimSpace(raw)
		if name == "" || !filepath.IsAbs(raw) {
			return nil, errors.New("spawn.pty: executable names and paths must be explicit")
		}
		resolved, err := exec.LookPath(raw)
		if err != nil {
			return nil, err
		}
		resolved, err = filepath.EvalSymlinks(resolved)
		if err != nil {
			return nil, err
		}
		executables[name] = filepath.Clean(resolved)
	}
	maxArgc, err := strconv.Atoi(grant.Attributes["max_argc"])
	if err != nil || maxArgc < 0 || maxArgc > 64 {
		return nil, errors.New("spawn.pty: max_argc must be between 0 and 64")
	}
	maxArgBytes, err := strconv.Atoi(grant.Attributes["max_arg_bytes"])
	if err != nil || maxArgBytes < 0 || maxArgBytes > 65536 {
		return nil, errors.New("spawn.pty: max_arg_bytes must be between 0 and 65536")
	}
	var env map[string]string
	if err := json.Unmarshal([]byte(grant.Attributes["environment_json"]), &env); err != nil {
		return nil, errors.New("spawn.pty: environment_json must be an object")
	}
	if len(env) > 32 {
		return nil, errors.New("spawn.pty: environment exceeds 32 entries")
	}
	environment := make([]string, 0, len(env))
	for key, value := range env {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, '\x00') || len(key)+len(value) > 8192 {
			return nil, errors.New("spawn.pty: invalid environment entry")
		}
		environment = append(environment, key+"="+value)
	}
	return &scopedPTYPolicy{root: filepath.Clean(root), executables: executables, environment: environment, maxArgc: maxArgc, maxArgBytes: maxArgBytes}, nil
}

func (p *scopedPTYPolicy) command(shell string, args []string, dir string) ([]string, string, []string, error) {
	shell = strings.TrimSpace(strings.ToLower(shell))
	executable, ok := p.executables[shell]
	if !ok {
		return nil, "", nil, errors.New("shell executable denied")
	}
	if len(args) > p.maxArgc {
		return nil, "", nil, errors.New("shell argv count denied")
	}
	total := 0
	for _, arg := range args {
		if strings.ContainsRune(arg, '\x00') {
			return nil, "", nil, errors.New("shell argv contains NUL")
		}
		total += len(arg)
	}
	if total > p.maxArgBytes {
		return nil, "", nil, errors.New("shell argv bytes denied")
	}
	if strings.TrimSpace(dir) == "" || strings.TrimSpace(dir) == "." {
		dir = p.root
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil || filepath.Clean(resolved) != p.root {
		return nil, "", nil, errors.New("terminal working root denied")
	}
	return append([]string{executable}, args...), p.root, append([]string(nil), p.environment...), nil
}
