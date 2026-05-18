package agenthooks

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Provider is the abstraction over a hosted agent's hook integration.
// flow speaks two natively (Claude, Codex); third parties (opencode,
// pi, grok, amp) can plug in by registering an additional Provider.
//
// Every Provider answers the same three questions:
//   1. Where in the workdir do my hooks live? (HookFile)
//   2. What lifecycle events do I register, with what matchers? (Events)
//   3. What command-line am I asking the host to execute? (HookCommand)
//
// The shared installer in local.go iterates over registered providers,
// each writing into its own file. That's the only way new providers are
// added — no edits to claudeHooks / codexHooks slices.
type Provider interface {
	// Name returns the lower-case provider identifier (e.g. "claude").
	// Used in log lines and as the --provider value on the registered
	// command. Must match the `provider` field on agent_runtime_states.
	Name() string

	// HookFile returns the absolute path of the per-workdir hook
	// config file for this provider. workDir is already absolute.
	HookFile(workDir string) string

	// HookCommand returns the literal command line to install under
	// each event. opts.CommandPath is the absolute flow binary path
	// (ignored — see hookCommands for why we always emit bare `flow`),
	// opts.HookURL points at the local UI server when known.
	HookCommand(opts InstallOptions) string

	// Events returns the list of (event, matcher) pairs to register.
	// The order is observed by tests asserting on file shape; keep it
	// stable.
	Events() []HookSpec

	// Extras returns per-hook-entry extras merged into every hook
	// object. Codex needs {"timeout": 3, "statusMessage": "..."};
	// Claude does not. Returning nil is fine.
	Extras() map[string]any
}

// HookSpec is one row of a Provider's Events() table.
type HookSpec struct {
	Event   string
	Matcher string
}

// providers is the global registry. Append at package init time.
// Order matters only for log determinism — installation runs them
// independently.
var providers = []Provider{
	claudeProvider{},
	codexProvider{},
}

// RegisterProvider appends a new Provider to the registry. Third-party
// integrations (e.g. an opencode plugin) can call this from their own
// package's init to plug in.
func RegisterProvider(p Provider) {
	if p == nil {
		return
	}
	providers = append(providers, p)
}

// Providers returns a snapshot of the registered providers. Useful
// when callers want to iterate the install matrix in their own loop
// (e.g. status reporting).
func Providers() []Provider {
	out := make([]Provider, len(providers))
	copy(out, providers)
	return out
}

// claudeProvider implements Provider for Claude Code's
// .claude/settings.local.json hook surface.
type claudeProvider struct{}

func (claudeProvider) Name() string { return "claude" }

func (claudeProvider) HookFile(workDir string) string {
	return filepath.Join(workDir, ".claude", "settings.local.json")
}

func (claudeProvider) HookCommand(opts InstallOptions) string {
	return buildHookCommand("claude", opts)
}

func (claudeProvider) Events() []HookSpec {
	return []HookSpec{
		{Event: "SessionStart", Matcher: "startup|resume"},
		{Event: "UserPromptSubmit"},
		{Event: "PermissionRequest"},
		{Event: "PermissionDenied"},
		{Event: "Notification"},
		{Event: "Elicitation"},
		{Event: "ElicitationResult"},
		{Event: "PreToolUse", Matcher: "AskUserQuestion|ExitPlanMode"},
		{Event: "PostToolUse"},
		{Event: "PostToolUseFailure"},
		{Event: "PostToolBatch"},
		{Event: "Stop"},
		{Event: "StopFailure"},
		{Event: "SessionEnd"},
		{Event: "TeammateIdle"},
		{Event: "SubagentStart"},
		{Event: "SubagentStop"},
		{Event: "TaskCreated"},
		{Event: "TaskCompleted"},
	}
}

func (claudeProvider) Extras() map[string]any { return nil }

// codexProvider implements Provider for Codex's .codex/hooks.json
// hook surface. Codex's hook entries support extra fields like timeout
// and statusMessage that Claude does not.
type codexProvider struct{}

func (codexProvider) Name() string { return "codex" }

func (codexProvider) HookFile(workDir string) string {
	return filepath.Join(workDir, ".codex", "hooks.json")
}

func (codexProvider) HookCommand(opts InstallOptions) string {
	return buildHookCommand("codex", opts)
}

func (codexProvider) Events() []HookSpec {
	return []HookSpec{
		{Event: "SessionStart", Matcher: "startup|resume|clear"},
		{Event: "UserPromptSubmit"},
		{Event: "PreToolUse"},
		{Event: "PermissionRequest"},
		{Event: "PostToolUse"},
		{Event: "Stop"},
	}
}

func (codexProvider) Extras() map[string]any {
	return map[string]any{"timeout": 3, "statusMessage": "Syncing flow status"}
}

// buildHookCommand is the single source of truth for the registered
// command line. Stamping --hook-version here means every provider
// (current and future) opts into version tagging without explicit code.
func buildHookCommand(provider string, opts InstallOptions) string {
	cmd := fmt.Sprintf("%s hook agent-event --provider %s --hook-version %d",
		installedFlowPath, provider, CurrentHookVersion)
	if hookURL := strings.TrimSpace(opts.HookURL); hookURL != "" {
		cmd += " --url " + shellQuoteArg(hookURL)
	}
	return cmd
}
