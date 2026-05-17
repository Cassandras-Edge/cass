package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Cassandras-Edge/cass/internal/cassconfig"
)

// claudeCmd / codexCmd are passthrough wrappers — `cass claude [args]`
// loads .cass.toml for the cwd, fires `cass refresh-keys` async (so
// near-expiry keys self-heal before they hit the wire), then EXECs the
// target CLI with config-derived args prepended to user args. config env
// is merged into the inherited environment. If the first user arg matches
// a configured persona name, that persona's args/env are layered between
// the base client defaults and the remaining user args.
//
// We replace the entire cass process via syscall.Exec so signals, exit
// codes, and tty state pass through cleanly — the user shouldn't be able
// to tell cass is in the loop other than via behavior.

func claudeCmd() *cobra.Command {
	return wrapperCmd("claude")
}

func codexCmd() *cobra.Command {
	return wrapperCmd("codex")
}

func wrapperCmd(client string) *cobra.Command {
	return &cobra.Command{
		Use:                client + " [args...]",
		Short:              "Run " + client + " with per-project .cass.toml defaults + MCP key auto-refresh",
		DisableFlagParsing: true, // pass all flags through to the wrapped CLI
		SilenceUsage:       true,
		RunE: func(_ *cobra.Command, args []string) error {
			return runWrapper(client, args)
		},
	}
}

func runWrapper(client string, userArgs []string) error {
	binPath, err := exec.LookPath(client)
	if err != nil {
		return fmt.Errorf("%s not found in PATH: %w", client, err)
	}

	cwd, _ := os.Getwd()
	resolved, err := cassconfig.LoadForCwd(cwd, client)
	if err != nil {
		// Don't fail the wrapper for a broken config — surface the error
		// and exec without overrides. The user's claude/codex invocation
		// is more important than our convenience layer.
		fmt.Fprintf(os.Stderr, "cass: ignoring %s: %v\n", client, err)
		resolved = &cassconfig.Resolved{}
	}

	// Background-refresh near-expiry MCP keys. We Start() and don't Wait:
	// the refresh either finishes in parallel with the user's session or
	// gets reparented to launchd/init when cass execs into the target.
	// `--quiet` is honored by refresh-keys (no output on healthy runs).
	go func() {
		c := exec.Command(os.Args[0], "refresh-keys", "--if-near-expiry", "--quiet")
		c.Stdout = nil
		c.Stderr = nil
		_ = c.Run()
	}()

	persona, remainingArgs := resolvePersona(resolved, userArgs)

	// Merge env: inherit current, then overlay config env vars, then persona
	// env. Persona values win over base client values.
	env := os.Environ()
	for k, v := range resolved.Env {
		env = append(env, k+"="+v)
	}
	for k, v := range persona.Env {
		env = append(env, k+"="+v)
	}

	// Build argv: config args go FIRST so user-supplied args can override
	// them in a left-wins fashion (claude/codex parse args left-to-right
	// and `--foo last-wins` is the common pattern). Persona args come after
	// base config args and before the user's remaining args, so a persona can
	// specialize defaults while still letting the user override at launch.
	// argv[0] must be the binary's own name, not the full path.
	argv := append([]string{client}, resolved.Args...)
	argv = append(argv, persona.Args...)
	argv = append(argv, remainingArgs...)

	// syscall.Exec replaces the cass process — does not return on success.
	if err := syscall.Exec(binPath, argv, env); err != nil {
		return fmt.Errorf("exec %s: %w", binPath, err)
	}
	return nil
}

func resolvePersona(resolved *cassconfig.Resolved, userArgs []string) (cassconfig.PersonaConfig, []string) {
	if resolved == nil || len(userArgs) == 0 || len(resolved.Personas) == 0 {
		return cassconfig.PersonaConfig{}, userArgs
	}
	name := userArgs[0]
	if name == "" || name[0] == '-' {
		return cassconfig.PersonaConfig{}, userArgs
	}
	persona, ok := resolved.Personas[name]
	if !ok {
		return cassconfig.PersonaConfig{}, userArgs
	}
	return persona, userArgs[1:]
}
