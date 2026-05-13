package claudecfg

import (
	"strings"
)

// AutoUpdateHookCommand is the SessionStart hook installed by cass setup
// so every Claude session rotates near-expiry keys best-effort. Fast
// no-op when keys are healthy. Marked `async` so it doesn't block
// session start.
//
// Manifest + skill refresh is intentionally NOT in the hook today —
// those change rarely, and re-running `cass setup` is the canonical
// way to pull updates. A future `cass sync` can fold both together.
const AutoUpdateHookCommand = "cass refresh-keys --if-near-expiry >/dev/null 2>&1 || true"

// EnsureAutoUpdateHook adds the cass auto-update SessionStart hook to the
// settings, if not already present. Returns true if it added the hook.
//
// settings.json hooks shape:
//
//	"hooks": {
//	  "SessionStart": [
//	    {"hooks": [{"type": "command", "command": "...", "timeout": 5, "async": true}]}
//	  ]
//	}
//
// Multiple SessionStart entries can coexist — we only add ours if no
// existing entry runs the cass auto-update command.
func EnsureAutoUpdateHook(settings map[string]any) bool {
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		settings["hooks"] = hooks
	}
	sessionStart, _ := hooks["SessionStart"].([]any)

	// Already installed?
	for _, group := range sessionStart {
		gm, ok := group.(map[string]any)
		if !ok {
			continue
		}
		entries, _ := gm["hooks"].([]any)
		for _, h := range entries {
			hm, ok := h.(map[string]any)
			if !ok {
				continue
			}
			cmd, _ := hm["command"].(string)
			if strings.Contains(cmd, "cass refresh-keys") || strings.Contains(cmd, "cass update") {
				return false
			}
		}
	}

	sessionStart = append(sessionStart, map[string]any{
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": AutoUpdateHookCommand,
				"timeout": 5,
				"async":   true,
			},
		},
	})
	hooks["SessionStart"] = sessionStart
	return true
}

// RemoveAutoUpdateHook strips any SessionStart entry whose command
// contains `cass update`. Used during teardown.
func RemoveAutoUpdateHook(settings map[string]any) bool {
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		return false
	}
	sessionStart, _ := hooks["SessionStart"].([]any)
	filtered := make([]any, 0, len(sessionStart))
	changed := false
	for _, group := range sessionStart {
		gm, ok := group.(map[string]any)
		if !ok {
			filtered = append(filtered, group)
			continue
		}
		entries, _ := gm["hooks"].([]any)
		keptEntries := make([]any, 0, len(entries))
		for _, h := range entries {
			hm, ok := h.(map[string]any)
			if !ok {
				keptEntries = append(keptEntries, h)
				continue
			}
			cmd, _ := hm["command"].(string)
			if strings.Contains(cmd, "cass refresh-keys") || strings.Contains(cmd, "cass update") {
				changed = true
				continue
			}
			keptEntries = append(keptEntries, h)
		}
		if len(keptEntries) == 0 {
			continue
		}
		gm["hooks"] = keptEntries
		filtered = append(filtered, gm)
	}
	if changed {
		if len(filtered) == 0 {
			delete(hooks, "SessionStart")
		} else {
			hooks["SessionStart"] = filtered
		}
	}
	return changed
}

// EnsureCookieSyncHook adds a per-service cookie sync to SessionStart.
// Each cookieSync entry runs `cass cookies sync <service> --no-open` in
// the background. This replaces the per-plugin ensure-cookies.sh from
// the old marketplace setup.
//
// Idempotent — checks if a hook for the same service already exists.
func EnsureCookieSyncHook(settings map[string]any, cookieService string) bool {
	if cookieService == "" {
		return false
	}
	cmd := cookieSyncCommand(cookieService)
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		settings["hooks"] = hooks
	}
	sessionStart, _ := hooks["SessionStart"].([]any)

	for _, group := range sessionStart {
		gm, ok := group.(map[string]any)
		if !ok {
			continue
		}
		entries, _ := gm["hooks"].([]any)
		for _, h := range entries {
			hm, ok := h.(map[string]any)
			if !ok {
				continue
			}
			existing, _ := hm["command"].(string)
			if existing == cmd {
				return false
			}
		}
	}

	sessionStart = append(sessionStart, map[string]any{
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": cmd,
				"timeout": 15,
				"async":   true,
			},
		},
	})
	hooks["SessionStart"] = sessionStart
	return true
}

func cookieSyncCommand(service string) string {
	return "cass cookies sync " + service + " --no-open >/dev/null 2>&1 || true"
}

// RemoveCookieSyncHook strips the SessionStart entry matching the given
// service's cookie sync command.
func RemoveCookieSyncHook(settings map[string]any, service string) bool {
	target := cookieSyncCommand(service)
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		return false
	}
	sessionStart, _ := hooks["SessionStart"].([]any)
	filtered := make([]any, 0, len(sessionStart))
	changed := false
	for _, group := range sessionStart {
		gm, ok := group.(map[string]any)
		if !ok {
			filtered = append(filtered, group)
			continue
		}
		entries, _ := gm["hooks"].([]any)
		keptEntries := make([]any, 0, len(entries))
		for _, h := range entries {
			hm, ok := h.(map[string]any)
			if !ok {
				keptEntries = append(keptEntries, h)
				continue
			}
			cmd, _ := hm["command"].(string)
			if cmd == target {
				changed = true
				continue
			}
			keptEntries = append(keptEntries, h)
		}
		if len(keptEntries) == 0 {
			continue
		}
		gm["hooks"] = keptEntries
		filtered = append(filtered, gm)
	}
	if changed {
		if len(filtered) == 0 {
			delete(hooks, "SessionStart")
		} else {
			hooks["SessionStart"] = filtered
		}
	}
	return changed
}
