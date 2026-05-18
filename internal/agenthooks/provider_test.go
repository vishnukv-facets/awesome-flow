package agenthooks

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProviderRegistryDefaults pins the built-in providers so a refactor
// can't accidentally drop one. Claude and Codex must always be in the
// default registry, in deterministic order.
func TestProviderRegistryDefaults(t *testing.T) {
	got := Providers()
	if len(got) < 2 {
		t.Fatalf("Providers() returned %d, want at least 2", len(got))
	}
	if got[0].Name() != "claude" {
		t.Fatalf("first provider = %s, want claude", got[0].Name())
	}
	if got[1].Name() != "codex" {
		t.Fatalf("second provider = %s, want codex", got[1].Name())
	}
}

// TestProviderHookCommandStampsVersion confirms every Provider emits a
// command that carries --hook-version CurrentHookVersion. Without this,
// future providers might forget to opt in.
func TestProviderHookCommandStampsVersion(t *testing.T) {
	for _, p := range Providers() {
		cmd := p.HookCommand(InstallOptions{HookURL: "http://127.0.0.1:8787/api/hooks/agent"})
		if v := HookVersionFromCommand(cmd); v != CurrentHookVersion {
			t.Errorf("%s command --hook-version = %d, want %d in: %q",
				p.Name(), v, CurrentHookVersion, cmd)
		}
		if !strings.Contains(cmd, "--provider "+p.Name()) {
			t.Errorf("%s command missing --provider %s: %q", p.Name(), p.Name(), cmd)
		}
		if !strings.Contains(cmd, "--url 'http://127.0.0.1:8787/api/hooks/agent'") {
			t.Errorf("%s command missing --url stamping: %q", p.Name(), cmd)
		}
	}
}

// TestCustomProviderInstall verifies the extension contract — a
// third-party Provider can register and InstallLocal will write its
// hook file alongside the built-ins without code changes to
// InstallLocalWithOptions.
func TestCustomProviderInstall(t *testing.T) {
	old := providers
	t.Cleanup(func() { providers = old })

	dir := t.TempDir()
	custom := stubProvider{
		name:    "stubby",
		relPath: ".stubby/hooks.json",
		events:  []HookSpec{{Event: "TestEvent"}},
	}
	RegisterProvider(custom)

	if _, err := InstallLocal(dir); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".stubby", "hooks.json"))
	if err != nil {
		t.Fatalf("custom provider hook file not written: %v", err)
	}
	if !strings.Contains(string(raw), "--provider stubby") {
		t.Fatalf("custom provider hook file missing provider stamp:\n%s", raw)
	}
}

type stubProvider struct {
	name    string
	relPath string
	events  []HookSpec
}

func (s stubProvider) Name() string { return s.name }
func (s stubProvider) HookFile(workDir string) string {
	return filepath.Join(workDir, s.relPath)
}
func (s stubProvider) HookCommand(opts InstallOptions) string {
	return fmt.Sprintf("flow hook agent-event --provider %s --hook-version %d",
		s.name, CurrentHookVersion)
}
func (s stubProvider) Events() []HookSpec    { return s.events }
func (s stubProvider) Extras() map[string]any { return nil }
