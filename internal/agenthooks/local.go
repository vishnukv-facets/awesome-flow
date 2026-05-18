package agenthooks

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	installedFlowPath = "flow"
	// CurrentHookVersion is bumped whenever the wire format or
	// per-hook behavior changes in a way that older installed copies
	// should re-install. Stamped into every registered hook command
	// as `--hook-version N`; the server reads it back from the hook
	// payload and surfaces an upgrade hint at the next SessionStart
	// when a session's command is below this value.
	CurrentHookVersion = 3
	ClaudeCommand      = installedFlowPath + " hook agent-event --provider claude"
	CodexCommand       = installedFlowPath + " hook agent-event --provider codex"
)

type InstallOptions struct {
	CommandPath string
	HookURL     string
}

func InstallLocal(workDir string) (bool, error) {
	return InstallLocalWithOptions(workDir, InstallOptions{})
}

// InstallLocalWithOptions installs (or upgrades) every registered
// Provider's hook entries into workDir. Adding a new agent host means
// implementing the Provider interface and registering it; this
// installer does not need to change.
func InstallLocalWithOptions(workDir string, opts InstallOptions) (bool, error) {
	root := strings.TrimSpace(workDir)
	if root == "" {
		return false, fmt.Errorf("workdir is empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return false, err
	}
	if st, err := os.Stat(abs); err != nil {
		return false, err
	} else if !st.IsDir() {
		return false, fmt.Errorf("%s is not a directory", abs)
	}

	changed := false
	for _, p := range Providers() {
		file := p.HookFile(abs)
		command := p.HookCommand(opts)
		extras := p.Extras()
		for _, hook := range p.Events() {
			added, err := installHook(file, hook.Event, hook.Matcher, command, extras)
			if err != nil {
				return changed, err
			}
			changed = changed || added
		}
	}
	if err := excludeLocalHookFiles(abs); err != nil {
		return changed, err
	}
	return changed, nil
}

func InstallKnownWorkdirs(db *sql.DB) (int, error) {
	return InstallKnownWorkdirsWithOptions(db, InstallOptions{})
}

func InstallKnownWorkdirsWithOptions(db *sql.DB, opts InstallOptions) (int, error) {
	if db == nil {
		return 0, nil
	}
	paths := map[string]bool{}
	queries := []string{
		`SELECT work_dir FROM tasks WHERE deleted_at IS NULL`,
		`SELECT worktree_path FROM tasks WHERE worktree_path IS NOT NULL AND worktree_path != '' AND deleted_at IS NULL`,
		`SELECT work_dir FROM projects WHERE deleted_at IS NULL`,
		`SELECT work_dir FROM playbooks WHERE deleted_at IS NULL`,
		`SELECT path FROM workdirs`,
	}
	for _, query := range queries {
		rows, err := db.Query(query)
		if err != nil {
			return 0, err
		}
		for rows.Next() {
			var path sql.NullString
			if err := rows.Scan(&path); err != nil {
				rows.Close()
				return 0, err
			}
			if path.Valid && strings.TrimSpace(path.String) != "" {
				paths[path.String] = true
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return 0, err
		}
		rows.Close()
	}

	changed := 0
	var errs []error
	for path := range paths {
		didChange, err := InstallLocalWithOptions(path, opts)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			errs = append(errs, err)
			continue
		}
		if didChange {
			changed++
		}
	}
	return changed, errors.Join(errs...)
}

// HookVersionFromCommand extracts the --hook-version N value from an
// installed hook command line, or 0 if the flag isn't present. Used by
// installation status reporting and by sameManagedAgentHookCommand
// matching, which must treat any flow-managed hook command as a
// candidate for in-place upgrade regardless of stamped version.
func HookVersionFromCommand(command string) int {
	fields := strings.Fields(command)
	for i, field := range fields {
		field = strings.Trim(field, `"'`)
		if field == "--hook-version" && i+1 < len(fields) {
			if n, err := strconv.Atoi(strings.Trim(fields[i+1], `"'`)); err == nil {
				return n
			}
			return 0
		}
		if rest, ok := strings.CutPrefix(field, "--hook-version="); ok {
			if n, err := strconv.Atoi(strings.Trim(rest, `"'`)); err == nil {
				return n
			}
			return 0
		}
	}
	return 0
}

func installHook(path, event, matcher, command string, extras map[string]any) (bool, error) {
	cfg, err := readHookConfig(path)
	if err != nil {
		return false, err
	}
	hooks, _ := cfg["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	entries, _ := hooks[event].([]any)
	keptEntries := make([]any, 0, len(entries))
	changed := false
	hasDesired := false
	desiredMatcher := strings.TrimSpace(matcher)
	for _, entry := range entries {
		m, ok := entry.(map[string]any)
		if !ok {
			keptEntries = append(keptEntries, entry)
			continue
		}
		entryMatcher, _ := m["matcher"].(string)
		entryMatcher = strings.TrimSpace(entryMatcher)
		inner, _ := m["hooks"].([]any)
		filtered := make([]any, 0, len(inner))
		for _, h := range inner {
			hm, ok := h.(map[string]any)
			if !ok {
				filtered = append(filtered, h)
				continue
			}
			cmd, _ := hm["command"].(string)
			if cmd == command {
				if entryMatcher != desiredMatcher {
					changed = true
					continue
				}
				hasDesired = true
				filtered = append(filtered, h)
				continue
			}
			if sameManagedAgentHookCommand(cmd, command) {
				changed = true
				continue
			}
			filtered = append(filtered, h)
		}
		if len(filtered) != len(inner) {
			m["hooks"] = filtered
		}
		if len(filtered) == 0 {
			changed = true
			continue
		}
		keptEntries = append(keptEntries, m)
	}
	entries = keptEntries
	if hasDesired {
		if changed {
			hooks[event] = entries
			cfg["hooks"] = hooks
			return true, writeHookConfig(path, cfg)
		}
		return false, nil
	}

	hookEntry := map[string]any{"type": "command", "command": command}
	for k, v := range extras {
		hookEntry[k] = v
	}
	group := map[string]any{"hooks": []any{hookEntry}}
	if strings.TrimSpace(matcher) != "" {
		group["matcher"] = matcher
	}
	entries = append(entries, group)
	hooks[event] = entries
	cfg["hooks"] = hooks
	return true, writeHookConfig(path, cfg)
}

func sameManagedAgentHookCommand(existing, desired string) bool {
	existing = strings.TrimSpace(existing)
	desired = strings.TrimSpace(desired)
	if existing == "" || desired == "" {
		return false
	}
	if !strings.Contains(existing, "hook agent-event") || !strings.Contains(desired, "hook agent-event") {
		return false
	}
	return hookProvider(existing) != "" && hookProvider(existing) == hookProvider(desired)
}

func hookProvider(command string) string {
	fields := strings.Fields(command)
	for i, field := range fields {
		field = strings.Trim(field, `"'`)
		if field == "--provider" && i+1 < len(fields) {
			return strings.Trim(fields[i+1], `"'`)
		}
		if strings.HasPrefix(field, "--provider=") {
			return strings.Trim(strings.TrimPrefix(field, "--provider="), `"'`)
		}
	}
	return ""
}

func shellQuoteArg(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func readHookConfig(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		return map[string]any{}, nil
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return cfg, nil
}

func writeHookConfig(path string, cfg map[string]any) error {
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}
	out = append(out, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func excludeLocalHookFiles(workDir string) error {
	excludePath, err := gitExcludePath(workDir)
	if err != nil || excludePath == "" {
		return nil
	}
	raw, err := os.ReadFile(excludePath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read git exclude %s: %w", excludePath, err)
	}
	existing := string(raw)
	lines := []string{".claude/settings.local.json", ".codex/hooks.json"}
	add := []string{}
	for _, line := range lines {
		if !containsExcludeLine(existing, line) {
			add = append(add, line)
		}
	}
	if len(add) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		return fmt.Errorf("mkdir git exclude dir %s: %w", filepath.Dir(excludePath), err)
	}
	prefix := ""
	if len(raw) > 0 && !strings.HasSuffix(existing, "\n") {
		prefix = "\n"
	}
	content := prefix + strings.Join(add, "\n") + "\n"
	f, err := os.OpenFile(excludePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open git exclude %s: %w", excludePath, err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		return fmt.Errorf("write git exclude %s: %w", excludePath, err)
	}
	return nil
}

func gitExcludePath(workDir string) (string, error) {
	cmd := exec.Command("git", "-C", workDir, "rev-parse", "--git-path", "info/exclude")
	out, err := cmd.Output()
	if err != nil {
		return "", nil
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		return "", nil
	}
	if filepath.IsAbs(path) {
		return path, nil
	}
	return filepath.Join(workDir, path), nil
}

func containsExcludeLine(content, line string) bool {
	for _, existing := range strings.Split(content, "\n") {
		if strings.TrimSpace(existing) == line {
			return true
		}
	}
	return false
}
