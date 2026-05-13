// Package registry lists every Cassandra service cass knows about.
//
// Each entry points cass at the GitHub repo where the service's
// `cass-manifest.json` lives. Adding a new service = append here + ship
// the manifest in that service's repo.
//
// The registry intentionally lives in source (vs. fetched at runtime) so
// `cass setup` works the first time on a clean machine — no chicken-and-egg
// "where does cass find the index of services" problem. The trade-off:
// adding a service requires a `cass` release, which is fine for a small
// internal platform where a service ships maybe monthly.
package registry

// Service is a Cassandra-managed unit cass knows how to install.
type Service struct {
	Name     string // canonical service ID (matches manifest.name + SERVICE_ID in the MCP)
	Repo     string // GitHub repo path, e.g. "Cassandras-Edge/cassandra-gmail-mcp"
	Optional bool   // true = skipped unless explicitly opted in via --with
}

// Services is the full catalog. Order is what's shown to the user in
// listings. Optional services don't get installed by default.
//
// IMPORTANT: Name must exactly match the FastMCP server's SERVICE_ID and
// the portal's MCP_SERVICES.id. /keys/validate does an exact-match lookup.
// Two historical mismatches: cassandra-media-mcp's SERVICE_ID is
// "yt-mcp" (legacy from when it was just transcription), and
// cassandra-routines exposes "routines" (the trailing -mcp is in the
// subdomain only).
var Services = []Service{
	// ── Default (installed unconditionally) ──
	{Name: "gmail-mcp", Repo: "Cassandras-Edge/cassandra-gmail-mcp"},
	{Name: "yt-mcp", Repo: "Cassandras-Edge/cassandra-media-mcp"},
	{Name: "twitter-mcp", Repo: "Cassandras-Edge/cassandra-twitter-mcp"},
	{Name: "reddit-mcp", Repo: "Cassandras-Edge/cassandra-reddit-mcp"},
	{Name: "discord-mcp", Repo: "Cassandras-Edge/cassandra-discord-mcp"},
	{Name: "market-research", Repo: "Cassandras-Edge/cassandra-market-research"},

	// ── Opt-in (--with <name> or --with all) ──
	{Name: "claudeai-mcp", Repo: "Cassandras-Edge/cassandra-claudeai-mcp", Optional: true},
	{Name: "gemini-mcp", Repo: "Cassandras-Edge/cassandra-gemini-mcp", Optional: true},
	{Name: "perplexity-mcp", Repo: "Cassandras-Edge/cassandra-perplexity-mcp", Optional: true},
	{Name: "tradingview-mcp", Repo: "Cassandras-Edge/cassandra-tradingview-mcp", Optional: true},
	{Name: "schwab-mcp", Repo: "Cassandras-Edge/cassandra-schwab-mcp", Optional: true},
	{Name: "routines", Repo: "Cassandras-Edge/cassandra-routines", Optional: true},
}

// Find returns the Service with the given name, or nil.
func Find(name string) *Service {
	for i := range Services {
		if Services[i].Name == name {
			return &Services[i]
		}
	}
	return nil
}

// Defaults returns the services installed without --with flags.
func Defaults() []Service {
	out := make([]Service, 0, len(Services))
	for _, s := range Services {
		if !s.Optional {
			out = append(out, s)
		}
	}
	return out
}

// Optionals returns the opt-in services.
func Optionals() []Service {
	out := make([]Service, 0, len(Services))
	for _, s := range Services {
		if s.Optional {
			out = append(out, s)
		}
	}
	return out
}
