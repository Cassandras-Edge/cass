package cmd

// Project-level .mcp.json rotation. Claude Code's "project" scope is a
// literal <project>/.mcp.json file — a separate store from ~/.claude.json,
// so keys registered there go stale silently when refresh-keys rotates
// everything else. This pass finds every .mcp.json Claude knows about
// (cwd + all .projects dirs in ~/.claude.json) and rewrites inline
// Authorization headers for freshly-keyed services. cass never creates
// these files; it only keeps existing ones current.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/Cassandras-Edge/cass/internal/claudecfg"
)

// mcpJSONPaths returns the deduped set of existing .mcp.json files:
// <cwd>/.mcp.json plus <dir>/.mcp.json for every project dir recorded
// in ~/.claude.json. Only paths that exist on disk are returned.
func mcpJSONPaths(store *claudecfg.MCPStore) []string {
	dirs := store.ProjectDirs()
	if cwd, err := os.Getwd(); err == nil {
		dirs = append(dirs, cwd)
	}
	seen := map[string]bool{}
	var out []string
	for _, dir := range dirs {
		path := filepath.Join(dir, ".mcp.json")
		if seen[path] {
			continue
		}
		seen[path] = true
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

// mcpJSONHas reports whether any of the given .mcp.json files registers
// a server under `name`. Malformed files are skipped (the rewrite pass
// warns about them).
func mcpJSONHas(paths []string, name string) bool {
	for _, path := range paths {
		doc, err := loadMCPJSON(path)
		if err != nil {
			continue
		}
		servers, _ := doc["mcpServers"].(map[string]any)
		if _, ok := servers[name]; ok {
			return true
		}
	}
	return false
}

// rewriteMCPJSONFiles patches the Authorization header of every
// freshly-keyed service found in the given .mcp.json files. Returns the
// paths actually modified. Failures warn but don't abort — the
// ~/.claude.json rotation already succeeded.
func rewriteMCPJSONFiles(paths []string, freshKeys map[string]string) []string {
	var touched []string
	for _, path := range paths {
		doc, err := loadMCPJSON(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  warning: load %s: %v\n", path, err)
			continue
		}
		servers, _ := doc["mcpServers"].(map[string]any)
		changed := false
		for svcName, newKey := range freshKeys {
			entry, ok := servers[svcName].(map[string]any)
			if !ok {
				continue
			}
			headers, _ := entry["headers"].(map[string]any)
			if headers == nil {
				headers = map[string]any{}
				entry["headers"] = headers
			}
			auth := "Bearer " + newKey
			if headers["Authorization"] == auth {
				continue
			}
			headers["Authorization"] = auth
			changed = true
		}
		if !changed {
			continue
		}
		if err := saveMCPJSON(path, doc); err != nil {
			fmt.Fprintf(os.Stderr, "  warning: write %s: %v\n", path, err)
			continue
		}
		touched = append(touched, path)
	}
	return touched
}

func loadMCPJSON(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("malformed: %w", err)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, nil
}

func saveMCPJSON(path string, doc map[string]any) error {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	return os.WriteFile(path, append(data, '\n'), mode)
}
