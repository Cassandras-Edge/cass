package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// codexHTTPServer is the on-disk shape of an [mcp_servers.<name>] entry that
// targets a streamable_http Cassandra service. Inline http_headers with a
// `Authorization: Bearer <key>` value gives codex everything it needs at
// startup — no env-var sourcing required (matches Claude's no-source UX).
type codexHTTPServer struct {
	URL         string            `toml:"url"`
	HTTPHeaders map[string]string `toml:"http_headers"`
}

// codexConfigPath resolves the right config.toml for the given scope:
//   - user:    $CODEX_HOME/config.toml (default ~/.codex/config.toml)
//   - project: <cwd>/.codex/config.toml
//
// For project scope we use the in-repo `.codex/config.toml` form, which codex
// auto-discovers via its "tree" / "repo" config layers when launched from
// inside the directory.
func codexConfigPath(scope string) (string, error) {
	switch scope {
	case "user":
		if home := os.Getenv("CODEX_HOME"); home != "" {
			return filepath.Join(home, "config.toml"), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".codex", "config.toml"), nil
	case "project":
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		return filepath.Join(cwd, ".codex", "config.toml"), nil
	default:
		return "", fmt.Errorf("unknown codex scope %q", scope)
	}
}

// loadCodexConfig reads the TOML file at path (treating ENOENT as empty),
// returning the decoded map. Decode errors surface so we don't silently
// clobber a config the user already hand-edited.
func loadCodexConfig(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	var out map[string]any
	if err := toml.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// saveCodexConfig writes the config back atomically with 0600 perms (it
// contains bearer tokens). Parent dirs are created with 0700.
func saveCodexConfig(path string, data map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	out, err := toml.Marshal(data)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// upsertCodexHTTPServer inserts/replaces the [mcp_servers.<name>] entry in
// the decoded config. It preserves any unrelated keys the user added under
// the same server (e.g. startup_timeout_sec, default_tools_approval_mode).
// Auth fields are always overwritten with the values we mint.
func upsertCodexHTTPServer(cfg map[string]any, name string, server codexHTTPServer) {
	servers, _ := cfg["mcp_servers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
		cfg["mcp_servers"] = servers
	}
	entry, _ := servers[name].(map[string]any)
	if entry == nil {
		entry = map[string]any{}
	}
	entry["url"] = server.URL
	// Drop the deprecated/orthogonal auth fields so re-runs don't leave a
	// stale bearer_token_env_var pointing at an unset var.
	delete(entry, "bearer_token")
	delete(entry, "bearer_token_env_var")
	delete(entry, "env_http_headers")
	// Merge into existing http_headers — preserve user-added headers but
	// always overwrite Authorization with the freshly minted key.
	headers, _ := entry["http_headers"].(map[string]any)
	if headers == nil {
		headers = map[string]any{}
	}
	for k, v := range server.HTTPHeaders {
		headers[k] = v
	}
	entry["http_headers"] = headers
	servers[name] = entry
}

// removeCodexServer deletes [mcp_servers.<name>] if present. Returns true if
// something was removed. If mcp_servers becomes empty, drops the table too.
func removeCodexServer(cfg map[string]any, name string) bool {
	servers, _ := cfg["mcp_servers"].(map[string]any)
	if servers == nil {
		return false
	}
	if _, ok := servers[name]; !ok {
		return false
	}
	delete(servers, name)
	if len(servers) == 0 {
		delete(cfg, "mcp_servers")
	}
	return true
}

// ensureCodexProjectTrust patches ~/.codex/config.toml so the user-scope
// config marks <projectPath> as trusted — without that flag codex disables
// the project's `.codex/config.toml` at load time, defeating per-project
// MCP registration entirely.
func ensureCodexProjectTrust(projectPath string) error {
	userPath, err := codexConfigPath("user")
	if err != nil {
		return err
	}
	cfg, err := loadCodexConfig(userPath)
	if err != nil {
		return err
	}
	projects, _ := cfg["projects"].(map[string]any)
	if projects == nil {
		projects = map[string]any{}
		cfg["projects"] = projects
	}
	entry, _ := projects[projectPath].(map[string]any)
	if entry == nil {
		entry = map[string]any{}
		projects[projectPath] = entry
	}
	if cur, _ := entry["trust_level"].(string); cur == "trusted" {
		return nil
	}
	entry["trust_level"] = "trusted"
	return saveCodexConfig(userPath, cfg)
}

// ensureCodexGitignore adds `.codex/` to <projectRoot>/.gitignore if the
// directory is a git repo and the entry isn't already there. Per-project
// .codex/config.toml contains bearer tokens, so we never want it committed.
func ensureCodexGitignore(projectRoot string) error {
	if _, err := os.Stat(filepath.Join(projectRoot, ".git")); err != nil {
		return nil // not a git repo, skip
	}
	gi := filepath.Join(projectRoot, ".gitignore")
	existing, _ := os.ReadFile(gi)
	for _, line := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(line) == ".codex/" {
			return nil
		}
	}
	suffix := ".codex/\n"
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		suffix = "\n.codex/\n"
	}
	f, err := os.OpenFile(gi, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(suffix)
	return err
}
