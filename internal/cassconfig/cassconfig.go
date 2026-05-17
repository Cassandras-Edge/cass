// Package cassconfig loads per-directory .cass.toml files that drive
// `cass claude` / `cass codex` wrappers — extra args + env vars to feed
// the underlying CLI based on what directory the user is in.
//
// Resolution is FIRST-FOUND-WINS up the directory tree, falling back to
// ~/.cass/config.toml when no .cass.toml exists in the path. This keeps
// the model simple: one file controls one directory subtree. To override
// at a child level, drop a more specific .cass.toml in that subdir.
//
// Schema:
//
//	[claude]
//	args = ["--dangerously-skip-permissions"]
//	[claude.env]
//	CLAUDE_CODE_EFFORT_LEVEL = "low"
//
//	[codex]
//	args = ["--ask-for-approval", "never"]
//
//	[codex.personas.finance]
//	args = ["--profile", "finance"]
package cassconfig

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

// File is the on-disk shape of a .cass.toml.
type File struct {
	Claude ClientConfig `toml:"claude"`
	Codex  ClientConfig `toml:"codex"`
}

// ClientConfig is the per-client section (same shape for claude and codex).
type ClientConfig struct {
	Args     []string                 `toml:"args"`
	Env      map[string]string        `toml:"env"`
	Personas map[string]PersonaConfig `toml:"personas"`
}

// PersonaConfig is a named args + env bundle under a client section.
//
// Example:
//
//	[codex.personas.finance]
//	args = ["--profile", "finance"]
//	[codex.personas.finance.env]
//	CASS_PERSONA = "finance"
type PersonaConfig struct {
	Args []string          `toml:"args"`
	Env  map[string]string `toml:"env"`
}

// Resolved is the effective config for one client, with provenance.
type Resolved struct {
	Args     []string
	Env      map[string]string
	Personas map[string]PersonaConfig
	Source   string // path to the .cass.toml we read; "" if no file found
}

const fileName = ".cass.toml"

// LoadForCwd walks `cwd` up to filesystem root looking for `.cass.toml`,
// falling back to ~/.cass/config.toml. Returns the resolved per-client
// config and which file it came from (or an empty Source when nothing
// was found, in which case both Args and Env are nil — exec falls
// through to the bare CLI).
func LoadForCwd(cwd, client string) (*Resolved, error) {
	path, err := findFile(cwd)
	if err != nil {
		return nil, err
	}
	if path == "" {
		return &Resolved{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var f File
	if err := toml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var section ClientConfig
	switch client {
	case "claude":
		section = f.Claude
	case "codex":
		section = f.Codex
	default:
		return nil, fmt.Errorf("unknown client %q (expected claude or codex)", client)
	}
	return &Resolved{
		Args:     section.Args,
		Env:      section.Env,
		Personas: section.Personas,
		Source:   path,
	}, nil
}

// findFile walks `start` up to /, returning the first .cass.toml it finds.
// Falls back to ~/.cass/config.toml when no project-scoped file exists.
func findFile(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(dir, fileName)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break // hit filesystem root
		}
		dir = parent
	}
	// Fall back to the user-global config.
	home, err := os.UserHomeDir()
	if err != nil {
		return "", nil
	}
	global := filepath.Join(home, ".cass", "config.toml")
	if info, err := os.Stat(global); err == nil && !info.IsDir() {
		return global, nil
	}
	return "", nil
}

// HomeGlobalPath returns ~/.cass/config.toml. Useful for `cass config edit`
// when the user wants to seed defaults globally.
func HomeGlobalPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cass", "config.toml"), nil
}

// Template is what `cass config init` writes — minimal, commented-out so
// the user sees the schema without forcing any default.
const Template = `# .cass.toml — per-directory overrides for ` + "`cass claude` / `cass codex`" + `.
# Walks cwd → parents → ~/.cass/config.toml (first found wins).
#
# Uncomment + edit the sections you want:

# [claude]
# args = ["--dangerously-skip-permissions"]
#
# [claude.env]
# CLAUDE_CODE_EFFORT_LEVEL = "low"

# [codex]
# args = []
#
# Named personas are explicit aliases. cass codex finance only expands
# when finance is declared here; otherwise args pass through unchanged.
#
# [codex.personas.finance]
# args = ["--profile", "finance"]
#
# [codex.personas.finance.env]
# CASS_PERSONA = "finance"
`

// WriteTemplate creates a fresh .cass.toml at the given path, refusing to
// overwrite an existing file.
func WriteTemplate(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("refusing to overwrite existing file: %s", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(Template), 0o644)
}
