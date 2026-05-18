package agenthooks

import (
	"encoding/json"
	"flow/internal/flowdb"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallLocalWritesRepoScopedHookFiles(t *testing.T) {
	dir := t.TempDir()
	changed, err := InstallLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("InstallLocal changed=false, want true on first install")
	}

	claude := readHookTestFile(t, filepath.Join(dir, ".claude", "settings.local.json"))
	if !hookFileHasFlowProvider(claude, "Notification", "claude") {
		t.Fatalf("Claude local hook missing Notification command: %#v", claude["hooks"])
	}
	if !hookFileHasFlowProvider(claude, "PermissionRequest", "claude") {
		t.Fatalf("Claude local hook missing PermissionRequest command: %#v", claude["hooks"])
	}
	if v := hookFileFirstFlowVersion(claude, "claude"); v != CurrentHookVersion {
		t.Fatalf("Claude hook --hook-version = %d, want %d", v, CurrentHookVersion)
	}

	codex := readHookTestFile(t, filepath.Join(dir, ".codex", "hooks.json"))
	if !hookFileHasFlowProvider(codex, "PermissionRequest", "codex") {
		t.Fatalf("Codex local hook missing PermissionRequest command: %#v", codex["hooks"])
	}
	if !hookFileHasFlowProvider(codex, "PreToolUse", "codex") {
		t.Fatalf("Codex local hook missing PreToolUse command: %#v", codex["hooks"])
	}
	if matcher, ok := hookMatcherForFlowProvider(codex, "PreToolUse", "codex"); !ok || matcher != "" {
		t.Fatalf("Codex PreToolUse matcher = %q found=%v, want broad managed hook without matcher", matcher, ok)
	}
	if !hookFileHasFlowProvider(codex, "PostToolUse", "codex") {
		t.Fatalf("Codex local hook missing PostToolUse command: %#v", codex["hooks"])
	}
	if v := hookFileFirstFlowVersion(codex, "codex"); v != CurrentHookVersion {
		t.Fatalf("Codex hook --hook-version = %d, want %d", v, CurrentHookVersion)
	}
}

func TestInstallLocalIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if changed, err := InstallLocal(dir); err != nil || !changed {
		t.Fatalf("first install changed=%v err=%v, want changed", changed, err)
	}
	if changed, err := InstallLocal(dir); err != nil || changed {
		t.Fatalf("second install changed=%v err=%v, want no change", changed, err)
	}
}

func TestInstallLocalPreservesExistingHooks(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"hooks":{"Notification":[{"hooks":[{"type":"command","command":"custom-notifier"}]}]},"theme":"dark"}`)
	if err := os.WriteFile(settings, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := InstallLocal(dir); err != nil {
		t.Fatal(err)
	}
	claude := readHookTestFile(t, settings)
	if !hookFileReferences(claude, "Notification", "custom-notifier") {
		t.Fatalf("existing hook was not preserved: %#v", claude["hooks"])
	}
	if !hookFileHasFlowProvider(claude, "Notification", "claude") {
		t.Fatalf("flow hook was not added: %#v", claude["hooks"])
	}
	if got, _ := claude["theme"].(string); got != "dark" {
		t.Fatalf("top-level field = %q, want dark", got)
	}
}

func TestInstallLocalWithOptionsUsesPathIndependentHookCommand(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin", "flow")
	hookURL := "http://127.0.0.1:8788/api/hooks/agent"

	changed, err := InstallLocalWithOptions(dir, InstallOptions{CommandPath: bin, HookURL: hookURL})
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("InstallLocalWithOptions changed=false, want true on first install")
	}

	codex := readHookTestFile(t, filepath.Join(dir, ".codex", "hooks.json"))
	if !hookFileHasFlowProvider(codex, "PreToolUse", "codex") {
		t.Fatalf("Codex hook missing managed command: %#v", codex["hooks"])
	}
	// The installed command must be path-independent (bare `flow ...`)
	// even when an absolute CommandPath was passed — we rely on PATH
	// rewriting at spawn time, not absolute-path baking.
	if hookFileMatchesAny(codex, func(cmd string) bool {
		return strings.HasPrefix(strings.TrimSpace(cmd), "'"+bin+"'")
	}) {
		t.Fatalf("Codex hook used absolute binary path: %#v", codex["hooks"])
	}
	if !hookFileMatchesAny(codex, func(cmd string) bool {
		return strings.Contains(cmd, "--url '"+hookURL+"'")
	}) {
		t.Fatalf("Codex hook missing --url stamping for %q: %#v", hookURL, codex["hooks"])
	}
}

func TestInstallLocalWithOptionsReplacesStaleManagedCommand(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"hooks":{"PreToolUse":[{"matcher":"request_user_input","hooks":[{"type":"command","command":"flow hook agent-event --provider codex --url 'http://old.invalid/api/hooks/agent'"}]}]}}`)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(dir, "bin", "flow")
	if changed, err := InstallLocalWithOptions(dir, InstallOptions{CommandPath: bin}); err != nil || !changed {
		t.Fatalf("InstallLocalWithOptions changed=%v err=%v, want replacement", changed, err)
	}
	codex := readHookTestFile(t, path)
	if hookFileReferences(codex, "PreToolUse", "flow hook agent-event --provider codex --url 'http://old.invalid/api/hooks/agent'") {
		t.Fatalf("stale command was not removed: %#v", codex["hooks"])
	}
	if !hookFileHasFlowProvider(codex, "PreToolUse", "codex") {
		t.Fatalf("replacement codex command missing: %#v", codex["hooks"])
	}
}

func TestInstallLocalWithOptionsReplacesStaleManagedMatcher(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"hooks":{"PreToolUse":[{"matcher":"AskUserQuestion|ExitPlanMode|request_user_input|mcp__.*request_user_input","hooks":[{"type":"command","command":"flow hook agent-event --provider codex --url 'http://127.0.0.1:8787/api/hooks/agent'"}]}]}}`)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	if changed, err := InstallLocalWithOptions(dir, InstallOptions{HookURL: "http://127.0.0.1:8787/api/hooks/agent"}); err != nil || !changed {
		t.Fatalf("InstallLocalWithOptions changed=%v err=%v, want matcher replacement", changed, err)
	}
	codex := readHookTestFile(t, path)
	matcher, ok := hookMatcherForFlowProvider(codex, "PreToolUse", "codex")
	if !ok || matcher != "" {
		t.Fatalf("Codex PreToolUse matcher = %q found=%v, want stale matcher removed: %#v", matcher, ok, codex["hooks"])
	}
	if changed, err := InstallLocalWithOptions(dir, InstallOptions{HookURL: "http://127.0.0.1:8787/api/hooks/agent"}); err != nil || changed {
		t.Fatalf("second install changed=%v err=%v, want idempotent", changed, err)
	}
}

func TestInstallLocalWithOptionsReplacesAbsoluteManagedCommand(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"hooks":{"SessionStart":[{"matcher":"startup|resume","hooks":[{"type":"command","command":"'/Users/vishnukv/facets/codebases/flow-manager/bin/flow' hook agent-event --provider claude --url 'http://127.0.0.1:8787/api/hooks/agent'"}]}]}}`)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	if changed, err := InstallLocalWithOptions(dir, InstallOptions{CommandPath: "/Users/vishnukv/facets/codebases/flow-manager/bin/flow", HookURL: "http://127.0.0.1:8787/api/hooks/agent"}); err != nil || !changed {
		t.Fatalf("InstallLocalWithOptions changed=%v err=%v, want replacement", changed, err)
	}
	claude := readHookTestFile(t, path)
	oldCommand := "'/Users/vishnukv/facets/codebases/flow-manager/bin/flow' hook agent-event --provider claude --url 'http://127.0.0.1:8787/api/hooks/agent'"
	if hookFileReferences(claude, "SessionStart", oldCommand) {
		t.Fatalf("absolute command was not removed: %#v", claude["hooks"])
	}
	if !hookFileHasFlowProvider(claude, "SessionStart", "claude") {
		t.Fatalf("replacement claude command missing: %#v", claude["hooks"])
	}
	if !hookFileMatchesAny(claude, func(cmd string) bool {
		return strings.Contains(cmd, "--url 'http://127.0.0.1:8787/api/hooks/agent'")
	}) {
		t.Fatalf("replacement claude command missing --url stamping: %#v", claude["hooks"])
	}
}

func TestInstallLocalExcludesGeneratedHookFilesFromGitStatus(t *testing.T) {
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", dir).CombinedOutput(); err != nil {
		t.Skipf("git init unavailable: %v: %s", err, out)
	}

	if _, err := InstallLocal(dir); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".git", "info", "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	exclude := string(raw)
	for _, want := range []string{".claude/settings.local.json", ".codex/hooks.json"} {
		if !strings.Contains(exclude, want) {
			t.Fatalf("git exclude missing %q:\n%s", want, exclude)
		}
	}
}

func TestInstallKnownWorkdirsAddsHooksForExistingRecords(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	taskDir := t.TempDir()
	worktreeDir := t.TempDir()
	projectDir := t.TempDir()
	playbookDir := t.TempDir()
	registryDir := t.TempDir()
	now := flowdb.NowISO()
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, work_dir, worktree_path, session_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"task", "Task", taskDir, worktreeDir, "session-1", now, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO projects (slug, name, work_dir, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)`,
		"project", "Project", projectDir, now, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO playbooks (slug, name, work_dir, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)`,
		"playbook", "Playbook", playbookDir, now, now,
	); err != nil {
		t.Fatal(err)
	}
	if err := flowdb.UpsertWorkdir(db, registryDir, "registry", "", ""); err != nil {
		t.Fatal(err)
	}

	changed, err := InstallKnownWorkdirs(db)
	if err != nil {
		t.Fatal(err)
	}
	if changed != 5 {
		t.Fatalf("changed = %d, want 5", changed)
	}
	for _, dir := range []string{taskDir, worktreeDir, projectDir, playbookDir, registryDir} {
		claude := readHookTestFile(t, filepath.Join(dir, ".claude", "settings.local.json"))
		if !hookFileHasFlowProvider(claude, "PermissionRequest", "claude") {
			t.Fatalf("%s missing Claude hook", dir)
		}
		codex := readHookTestFile(t, filepath.Join(dir, ".codex", "hooks.json"))
		if !hookFileHasFlowProvider(codex, "PermissionRequest", "codex") {
			t.Fatalf("%s missing Codex hook", dir)
		}
	}

	changed, err = InstallKnownWorkdirs(db)
	if err != nil {
		t.Fatal(err)
	}
	if changed != 0 {
		t.Fatalf("second install changed = %d, want 0", changed)
	}
}

func readHookTestFile(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse %s: %v\n%s", path, err, raw)
	}
	return cfg
}

func hookFileReferences(cfg map[string]any, event, command string) bool {
	_, ok := hookMatcherForCommand(cfg, event, command)
	return ok
}

func hookMatcherForCommand(cfg map[string]any, event, command string) (string, bool) {
	hooks, _ := cfg["hooks"].(map[string]any)
	entries, _ := hooks[event].([]any)
	for _, entry := range entries {
		m, _ := entry.(map[string]any)
		matcher, _ := m["matcher"].(string)
		inner, _ := m["hooks"].([]any)
		for _, hook := range inner {
			hm, _ := hook.(map[string]any)
			if cmd, _ := hm["command"].(string); cmd == command {
				return matcher, true
			}
		}
	}
	return "", false
}

// hookFileHasFlowProvider returns true when the given event has at least
// one flow-managed agent-event hook for the named provider, regardless
// of trailing args (--hook-version, --url, etc).
func hookFileHasFlowProvider(cfg map[string]any, event, provider string) bool {
	_, ok := hookMatcherForFlowProvider(cfg, event, provider)
	return ok
}

func hookMatcherForFlowProvider(cfg map[string]any, event, provider string) (string, bool) {
	hooks, _ := cfg["hooks"].(map[string]any)
	entries, _ := hooks[event].([]any)
	for _, entry := range entries {
		m, _ := entry.(map[string]any)
		matcher, _ := m["matcher"].(string)
		inner, _ := m["hooks"].([]any)
		for _, hook := range inner {
			hm, _ := hook.(map[string]any)
			cmd, _ := hm["command"].(string)
			if strings.Contains(cmd, "hook agent-event") && hookProvider(cmd) == provider {
				return matcher, true
			}
		}
	}
	return "", false
}

// hookFileMatchesAny returns true if any installed command (across all
// events) satisfies match. Used to assert on stamped suffixes (--url,
// absolute paths) without depending on the exact command formatting.
func hookFileMatchesAny(cfg map[string]any, match func(cmd string) bool) bool {
	hooks, _ := cfg["hooks"].(map[string]any)
	for _, entriesRaw := range hooks {
		entries, _ := entriesRaw.([]any)
		for _, entry := range entries {
			m, _ := entry.(map[string]any)
			inner, _ := m["hooks"].([]any)
			for _, hook := range inner {
				hm, _ := hook.(map[string]any)
				if cmd, _ := hm["command"].(string); match(cmd) {
					return true
				}
			}
		}
	}
	return false
}

func hookFileFirstFlowVersion(cfg map[string]any, provider string) int {
	hooks, _ := cfg["hooks"].(map[string]any)
	for _, entriesRaw := range hooks {
		entries, _ := entriesRaw.([]any)
		for _, entry := range entries {
			m, _ := entry.(map[string]any)
			inner, _ := m["hooks"].([]any)
			for _, hook := range inner {
				hm, _ := hook.(map[string]any)
				cmd, _ := hm["command"].(string)
				if strings.Contains(cmd, "hook agent-event") && hookProvider(cmd) == provider {
					return HookVersionFromCommand(cmd)
				}
			}
		}
	}
	return 0
}
