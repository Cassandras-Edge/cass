package claudecfg

// MCP servers in current Claude Code (v2.x) live in ~/.claude.json — NOT
// in .claude/settings.json (which is for hooks/permissions/pluginConfigs).
// The split:
//
//   ~/.claude.json
//     ├── .mcpServers                            ← user scope
//     └── .projects[<abs_cwd>].mcpServers        ← project-bound scope
//                                                  (Claude's "local"
//                                                  scope semantically:
//                                                  loads only when claude
//                                                  is run from <abs_cwd>)
//
//   <cwd>/.mcp.json                              ← Claude's "project"
//                                                  scope, designed to be
//                                                  committed to git. Not
//                                                  written by cass today
//                                                  because inline bearer
//                                                  tokens shouldn't be
//                                                  team-shared.
//
// This file owns just the ~/.claude.json side. It's surgical — only
// touches the two map keys above, preserving every other field in the
// file (lastSessionId, autoUpdates, anonymousId, etc — all the Claude
// state we mustn't trample).

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// MCPStore wraps the entire ~/.claude.json document. Load to get a
// handle, mutate via Upsert/Remove, then Save to write back.
type MCPStore struct {
	path string
	doc  map[string]any
}

// ClaudeJSONPath returns the absolute path to ~/.claude.json.
func ClaudeJSONPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude.json"), nil
}

// LoadMCPStore reads ~/.claude.json (empty doc if missing). Failures
// from a malformed file are fatal — better to abort than silently
// rewrite a broken file with our partial view.
func LoadMCPStore() (*MCPStore, error) {
	path, err := ClaudeJSONPath()
	if err != nil {
		return nil, err
	}
	s := &MCPStore{path: path, doc: map[string]any{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, &s.doc); err != nil {
		return nil, fmt.Errorf("%s malformed: %w", path, err)
	}
	if s.doc == nil {
		s.doc = map[string]any{}
	}
	return s, nil
}

// Save writes the (possibly mutated) document back to disk. Preserves
// the existing file mode if there is one, falling back to 0600 — the
// file contains bearer tokens and other secrets so a permissive default
// is unsafe.
func (s *MCPStore) Save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.doc, "", "  ")
	if err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if info, err := os.Stat(s.path); err == nil {
		mode = info.Mode().Perm()
	}
	return os.WriteFile(s.path, append(data, '\n'), mode)
}

// section returns the mcpServers map for the given scope, creating the
// nested structure on the fly. Returns the map itself plus a setter
// that ensures the change is reflected in s.doc (Go map vs. interface
// boxing — we hand back a closure to avoid the caller forgetting).
func (s *MCPStore) section(scope Scope) (map[string]any, error) {
	switch scope {
	case ScopeUser:
		servers, _ := s.doc["mcpServers"].(map[string]any)
		if servers == nil {
			servers = map[string]any{}
			s.doc["mcpServers"] = servers
		}
		return servers, nil

	case ScopeProject:
		// Claude calls this "local" — per-project, private to this user.
		// Stored at .projects[<abs_cwd>].mcpServers.
		cwd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		abs, err := filepath.Abs(cwd)
		if err != nil {
			return nil, err
		}
		projects, _ := s.doc["projects"].(map[string]any)
		if projects == nil {
			projects = map[string]any{}
			s.doc["projects"] = projects
		}
		proj, _ := projects[abs].(map[string]any)
		if proj == nil {
			proj = map[string]any{}
			projects[abs] = proj
		}
		servers, _ := proj["mcpServers"].(map[string]any)
		if servers == nil {
			servers = map[string]any{}
			proj["mcpServers"] = servers
		}
		return servers, nil

	default:
		return nil, fmt.Errorf("unsupported MCP scope %q (use user or project)", scope)
	}
}

// UpsertMCP registers or updates a single MCP server entry in the
// chosen scope. Idempotent — re-writing with the same spec is a no-op
// diff once Save runs.
func (s *MCPStore) UpsertMCP(scope Scope, name string, spec MCPServerSpec) error {
	servers, err := s.section(scope)
	if err != nil {
		return err
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
	return nil
}

// RemoveMCP deletes a server entry from the chosen scope if present.
// Returns whether anything was actually removed.
func (s *MCPStore) RemoveMCP(scope Scope, name string) (bool, error) {
	servers, err := s.section(scope)
	if err != nil {
		return false, err
	}
	if _, ok := servers[name]; !ok {
		return false, nil
	}
	delete(servers, name)
	return true, nil
}

// HasMCP returns true if `name` is registered in the chosen scope.
func (s *MCPStore) HasMCP(scope Scope, name string) (bool, error) {
	servers, err := s.section(scope)
	if err != nil {
		return false, err
	}
	_, ok := servers[name]
	return ok, nil
}

// ListMCPs returns the names registered in the chosen scope (sorted is
// the caller's job).
func (s *MCPStore) ListMCPs(scope Scope) ([]string, error) {
	servers, err := s.section(scope)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(servers))
	for k := range servers {
		out = append(out, k)
	}
	return out, nil
}

// MCPEntry retrieves a single server's stored spec by name + scope.
// Used by refresh-keys to read the existing URL when rotating a key.
func (s *MCPStore) MCPEntry(scope Scope, name string) (MCPServerSpec, bool, error) {
	servers, err := s.section(scope)
	if err != nil {
		return MCPServerSpec{}, false, err
	}
	entry, ok := servers[name].(map[string]any)
	if !ok {
		return MCPServerSpec{}, false, nil
	}
	spec := MCPServerSpec{
		Type: stringField(entry, "type"),
		URL:  stringField(entry, "url"),
	}
	if hdrs, ok := entry["headers"].(map[string]any); ok {
		spec.Headers = map[string]string{}
		for k, v := range hdrs {
			if s, ok := v.(string); ok {
				spec.Headers[k] = s
			}
		}
	}
	return spec, true, nil
}

func stringField(m map[string]any, k string) string {
	v, _ := m[k].(string)
	return v
}
