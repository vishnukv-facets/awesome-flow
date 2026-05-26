package app

import (
	"database/sql"
	"flow/internal/flowdb"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// cmdSpawn implements `flow spawn <name> [--parent <slug>] [--prompt <text>]
// [--project <slug>] [--work-dir <path>] [--slug <s>] [--priority h|m|l]
// [--agent claude|codex] [--dangerously-skip-permissions] [--no-open]`.
//
// It is a thin composition over `flow add task` + `flow do` with two
// extras:
//   - sets tasks.parent_slug when --parent is given, so the child appears
//     under its parent in the UI tree and `flow show task` reports the
//     linkage.
//   - rewrites the freshly-created brief.md from --prompt, so the spawned
//     agent starts with the parent's instruction inline (no second-turn
//     interview).
//
// --parent is OPTIONAL. With no --parent, spawn doubles as a fast
// non-interactive "create + open" for solo tasks: useful in scripts.
//
// --no-open lets a caller create the task without spawning a tab; flow
// do later is still possible by slug.
func cmdSpawn(args []string) int {
	fs := flagSet("spawn")
	parent := fs.String("parent", "", "slug of the parent task (this spawn becomes its child)")
	prompt := fs.String("prompt", "", "initial instruction for the spawned agent (replaces brief.md What section)")
	project := fs.String("project", "", "project slug to attach the new task to")
	slugFlag := fs.String("slug", "", "short user-chosen slug (default: derived from name)")
	workDir := fs.String("work-dir", "", "absolute path to the task's work_dir")
	mkdir := fs.Bool("mkdir", false, "create --work-dir if it does not exist")
	priority := fs.String("priority", "medium", "high|medium|low")
	agentFlag := fs.String("agent", "", "session agent: claude or codex")
	codexAgent := fs.Bool("codex", false, "shortcut for --agent codex")
	claudeAgent := fs.Bool("claude", false, "shortcut for --agent claude")
	permission := fs.String("permission-mode", flowdb.DefaultPermissionMode, "default|auto|bypass")
	dangerSkip := fs.Bool("dangerously-skip-permissions", false, "pass low-friction permissions flag through to the agent")
	noOpen := fs.Bool("no-open", false, "create the task but don't spawn a session yet")
	if leadingHelpArg(args) {
		fs.Usage()
		return 0
	}
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(os.Stderr, "error: spawn requires a name")
		return 2
	}
	name := args[0]
	if handled, rc := parseFlagSet(fs, args[1:]); handled {
		return rc
	}

	dbPath, err := flowDBPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	db, err := flowdb.OpenDB(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	// Validate parent up front so we fail before creating an orphaned
	// task row that the caller would need to clean up.
	parentSlug := strings.TrimSpace(*parent)
	if parentSlug != "" {
		if _, err := flowdb.GetTask(db, parentSlug); err != nil {
			db.Close()
			fmt.Fprintf(os.Stderr, "error: parent task %q not found\n", parentSlug)
			return 1
		}
	}
	db.Close()

	// Re-use cmdAdd's task creation path so naming, slugging, work_dir
	// resolution, brief stub, and workdir registry all stay consistent.
	addArgs := []string{"task", name, "--priority", *priority, "--permission-mode", *permission}
	if *slugFlag != "" {
		addArgs = append(addArgs, "--slug", *slugFlag)
	}
	if *project != "" {
		addArgs = append(addArgs, "--project", *project)
	}
	if *workDir != "" {
		addArgs = append(addArgs, "--work-dir", *workDir)
	}
	if *mkdir {
		addArgs = append(addArgs, "--mkdir")
	}
	if *codexAgent || strings.EqualFold(*agentFlag, "codex") {
		addArgs = append(addArgs, "--agent", "codex")
	} else if *claudeAgent || strings.EqualFold(*agentFlag, "claude") {
		addArgs = append(addArgs, "--agent", "claude")
	}
	if rc := cmdAdd(addArgs); rc != 0 {
		return rc
	}

	// cmdAdd already printed its success line. Resolve the slug it picked
	// (the slug flag may have been blank). Re-open the DB and search.
	db, err = flowdb.OpenDB(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	defer db.Close()
	createdSlug, err := resolveJustCreatedTaskSlug(db, name, *slugFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: locate spawned task: %v\n", err)
		return 1
	}

	// Record parent linkage. cmdAdd doesn't write it because parent linkage
	// is spawn-specific. AddTaskParent inserts the row into task_dependencies
	// and mirrors the first parent into tasks.parent_slug for backwards
	// compat. updated_at is bumped separately.
	if parentSlug != "" {
		if err := flowdb.AddTaskParent(db, createdSlug, parentSlug); err != nil {
			fmt.Fprintf(os.Stderr, "warning: set parent: %v\n", err)
		}
		if _, err := db.Exec(
			`UPDATE tasks SET updated_at = ? WHERE slug = ?`,
			flowdb.NowISO(), createdSlug,
		); err != nil {
			fmt.Fprintf(os.Stderr, "warning: bump updated_at: %v\n", err)
		}
	}

	// Rewrite the stub brief so the spawned agent sees the prompt inline.
	// We keep the stub's section headings (What/Why/Where/Done when/...)
	// so the agent still gets the bootstrap re-read instructions, but
	// drop a real "What" body in place of the placeholder.
	if strings.TrimSpace(*prompt) != "" {
		if err := writeSpawnedBrief(createdSlug, name, *prompt, parentSlug); err != nil {
			fmt.Fprintf(os.Stderr, "warning: write spawned brief: %v\n", err)
		}
	}

	if *noOpen {
		fmt.Printf("Spawned %s (not opened — pass --no-open=false or run `flow do %s` later)\n", createdSlug, createdSlug)
		return 0
	}

	doArgs := []string{createdSlug}
	if *dangerSkip {
		doArgs = append(doArgs, "--dangerously-skip-permissions")
	}
	if *codexAgent || strings.EqualFold(*agentFlag, "codex") {
		doArgs = append(doArgs, "--agent", "codex")
	}
	return cmdDo(doArgs)
}

// resolveJustCreatedTaskSlug finds the slug cmdAdd picked. If the user
// supplied --slug, that's the answer. Otherwise scan the most recently
// created task whose name matches.
func resolveJustCreatedTaskSlug(db *sql.DB, name, slugFlag string) (string, error) {
	if strings.TrimSpace(slugFlag) != "" {
		return slugFlag, nil
	}
	row := db.QueryRow(
		`SELECT slug FROM tasks WHERE name = ? ORDER BY created_at DESC LIMIT 1`,
		name,
	)
	var slug string
	if err := row.Scan(&slug); err != nil {
		return "", err
	}
	return slug, nil
}

// writeSpawnedBrief rewrites the freshly-created brief.md to inline the
// spawn prompt as the "What" section. Preserves the rest of the stub
// (Why/Where/Done when/...) so the bootstrap contract still applies.
func writeSpawnedBrief(slug, name, prompt, parentSlug string) error {
	root, err := flowRoot()
	if err != nil {
		return err
	}
	briefPath := filepath.Join(root, "tasks", slug, "brief.md")
	provenance := ""
	if parentSlug != "" {
		provenance = "\n*Spawned from parent task: `" + parentSlug + "`*\n"
	}
	body := "# " + name + "\n" +
		provenance + "\n" +
		"## What\n" +
		strings.TrimSpace(prompt) + "\n\n" +
		"## Why\n*Deferred — fill in at task start.*\n\n" +
		"## Where\nwork_dir: (set by add task)\n\n" +
		"## Done when\n*Deferred — fill in at task start.*\n\n" +
		"## Out of scope\n*Deferred*\n\n" +
		"## Open questions\n*Deferred*\n\n" +
		"---\n" +
		"*This brief was generated by `flow spawn`. The What section above " +
		"is the parent's instruction; everything else is deferred until the " +
		"spawned agent's first turn (see flow skill §9 deferred-section prompt).*\n"
	return os.WriteFile(briefPath, []byte(body), 0o644)
}
