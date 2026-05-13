// Package manifest defines + fetches the cass-manifest.json each Cassandra
// service ships at the root of its GitHub repo.
//
// The manifest is the contract between a service and cass — it tells cass
// how to wire the service into Claude Code / Codex settings, where to find
// the skill markdown, and what (if any) per-session bootstrap is needed
// (e.g. cookie sync for services backed by Firefox session cookies).
package manifest

// Manifest is the JSON shape of cass-manifest.json. All fields except
// `name` are optional — a manifest with only `name` + `description` is
// valid for a pure-doc plugin (no MCP server, no skill).
type Manifest struct {
	// Name is the canonical service ID, matches the MCP's SERVICE_ID.
	Name string `json:"name"`

	// Version is informational — `cass status` shows it but nothing depends
	// on it for routing. Semver expected.
	Version string `json:"version,omitempty"`

	// Description is a one-line summary, shown in listings.
	Description string `json:"description,omitempty"`

	// MCP, if set, registers this service as an MCP server in
	// ~/.claude/settings.json + ~/.codex/config.toml.
	MCP *MCPConfig `json:"mcp,omitempty"`

	// CookieSync, if set, names a `cass cookies` service whose Firefox
	// session cookies must be fresh for the MCP server to work. cass
	// installs a SessionStart hook that calls `cass cookies sync <name>`
	// best-effort on every Claude Code session.
	CookieSync string `json:"cookieSync,omitempty"`

	// Skill, if set, tells cass to drop a SKILL.md into
	// ~/.claude/skills/<name>/ from the repo. Optional and usually
	// unnecessary for pure-MCP services — the MCP server's own
	// `instructions` string already flows to the model. Useful when the
	// service has no MCP component (slash-command-only plugins) or
	// wants to advertise itself in the system-prompt skills list before
	// any MCP session has initialized.
	Skill *SkillConfig `json:"skill,omitempty"`
}

// MCPConfig describes how cass should expose the service's MCP server to
// MCP-aware clients.
type MCPConfig struct {
	// URL is the public MCP endpoint, e.g.
	// "https://twitter-mcp.cassandrasedge.com/mcp".
	URL string `json:"url"`

	// Auth is the bearer-token kind. Only "mcp_key" is supported today —
	// cass writes `Authorization: Bearer <mcp_key>`. Future kinds might be
	// "workos_oauth" or "service_token".
	Auth string `json:"auth"`
}

// SkillConfig points at the SKILL.md inside the service repo.
type SkillConfig struct {
	// Path is relative to the repo root. Defaults to "SKILL.md".
	Path string `json:"path,omitempty"`
}

// SkillPath returns the configured path or the default ("SKILL.md").
func (m *Manifest) SkillPath() string {
	if m.Skill == nil || m.Skill.Path == "" {
		return "SKILL.md"
	}
	return m.Skill.Path
}

// HasMCP returns true if the manifest registers an MCP server.
func (m *Manifest) HasMCP() bool {
	return m.MCP != nil && m.MCP.URL != ""
}

// HasSkill returns true if the manifest ships skill markdown.
func (m *Manifest) HasSkill() bool {
	return m.Skill != nil
}
