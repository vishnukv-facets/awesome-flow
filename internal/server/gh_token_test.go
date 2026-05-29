package server

import (
	"testing"
)

func TestTerminalEnvInjectsGHToken(t *testing.T) {
	orig := ghAuthToken
	t.Cleanup(func() { ghAuthToken = orig })

	ghAuthToken = func() string { return "gho_testtoken" }
	env := terminalEnv("", "")
	if got := envValueLocal(env, "GH_TOKEN"); got != "gho_testtoken" {
		t.Fatalf("GH_TOKEN = %q, want gho_testtoken", got)
	}

	// Empty token → no GH_TOKEN injected.
	ghAuthToken = func() string { return "" }
	env = terminalEnv("", "")
	if got := envValueLocal(env, "GH_TOKEN"); got != "" {
		t.Fatalf("GH_TOKEN = %q, want empty when gh has no token", got)
	}

	// Pre-existing token in env is not overwritten.
	ghAuthToken = func() string { return "gho_resolved" }
	t.Setenv("GH_TOKEN", "gho_preset")
	env = terminalEnv("", "")
	if got := envValueLocal(env, "GH_TOKEN"); got != "gho_preset" {
		t.Fatalf("GH_TOKEN = %q, want preset value preserved", got)
	}
}
