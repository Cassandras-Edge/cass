// Package claudecfg owns all reads + writes to ~/.claude/settings.json
// (or scope-equivalents) and the ~/.claude/skills/ tree. cass uses these
// helpers to register MCP servers, SKILL.md files, and SessionStart hooks
// directly — no plugin marketplace layer.
package claudecfg

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Scope selects which settings.json to touch.
type Scope string

const (
	ScopeUser    Scope = "user"
	ScopeProject Scope = "project"
	ScopeLocal   Scope = "local"
)

// SettingsPath returns the absolute path to the settings.json for the
// chosen scope. User scope uses ~/.claude/settings.json; project uses
// <cwd>/.claude/settings.json; local uses <cwd>/.claude/settings.local.json.
func SettingsPath(scope Scope) (string, error) {
	switch scope {
	case ScopeUser:
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".claude", "settings.json"), nil
	case ScopeProject:
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		return filepath.Join(cwd, ".claude", "settings.json"), nil
	case ScopeLocal:
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		return filepath.Join(cwd, ".claude", "settings.local.json"), nil
	default:
		return "", fmt.Errorf("unknown scope: %s", scope)
	}
}

// LoadSettings reads the settings.json for `scope`, returning an empty
// object if the file does not exist yet.
func LoadSettings(scope Scope) (map[string]any, error) {
	path, err := SettingsPath(scope)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("%s malformed: %w", path, err)
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// SaveSettings writes settings.json at `scope`, creating parent dirs as
// needed. Pretty-printed with 2-space indent for readability + diffs.
func SaveSettings(scope Scope, settings map[string]any) error {
	path, err := SettingsPath(scope)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// MCPServerSpec is the canonical shape cass writes under
// settings.json:mcpServers[name]. Matches Claude Code's documented schema.
type MCPServerSpec struct {
	Type    string            `json:"type"` // always "http" for our services
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

// UpsertMCPServer registers or updates a single MCP server entry in
// settings.json. Idempotent — re-writing with the same spec is a no-op
// diff. Returns whether the file actually changed (caller may want to
// skip a save when nothing's different, but for simplicity we re-marshal
// always; the diff check is informational).
func UpsertMCPServer(settings map[string]any, name string, spec MCPServerSpec) {
	servers, _ := settings["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
		settings["mcpServers"] = servers
	}
	entry := map[string]any{
		"type": spec.Type,
		"url":  spec.URL,
	}
	if len(spec.Headers) > 0 {
		headers := map[string]any{}
		for k, v := range spec.Headers {
			headers[k] = v
		}
		entry["headers"] = headers
	}
	servers[name] = entry
}

// RemoveMCPServer deletes an entry from settings.mcpServers, if present.
func RemoveMCPServer(settings map[string]any, name string) bool {
	servers, _ := settings["mcpServers"].(map[string]any)
	if servers == nil {
		return false
	}
	if _, ok := servers[name]; !ok {
		return false
	}
	delete(servers, name)
	return true
}

// HasMCPServer returns true if the named server is present.
func HasMCPServer(settings map[string]any, name string) bool {
	servers, _ := settings["mcpServers"].(map[string]any)
	if servers == nil {
		return false
	}
	_, ok := servers[name]
	return ok
}

// ListMCPServers returns the names of all registered MCP servers.
func ListMCPServers(settings map[string]any) []string {
	servers, _ := settings["mcpServers"].(map[string]any)
	out := make([]string, 0, len(servers))
	for k := range servers {
		out = append(out, k)
	}
	return out
}
