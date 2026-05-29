package server

import (
	"strings"
	"testing"
)

func TestCodexPermissionArgsEnablesNetwork(t *testing.T) {
	const net = "sandbox_workspace_write.network_access=true"
	hasNet := func(args []string) bool {
		return strings.Contains(strings.Join(args, " "), "-c "+net)
	}
	for _, mode := range []string{"auto", "default", ""} {
		args := codexPermissionArgs(mode)
		if !strings.Contains(strings.Join(args, " "), "--sandbox workspace-write") {
			t.Errorf("mode %q: expected workspace-write sandbox, got %v", mode, args)
		}
		if !hasNet(args) {
			t.Errorf("mode %q: expected network override, got %v", mode, args)
		}
	}
	// bypass runs unsandboxed — no override needed (or wanted).
	bypass := codexPermissionArgs("bypass")
	if hasNet(bypass) {
		t.Errorf("bypass should not carry the sandbox network override: %v", bypass)
	}
}
