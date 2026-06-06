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
	Name        string // canonical service ID (matches manifest.name + SERVICE_ID in the MCP)
	Repo        string // GitHub repo path, e.g. "Cassandras-Edge/cassandra-gmail-mcp"
	Optional    bool   // true = skipped unless explicitly opted in via --with
	Description string // one-line, shown in the interactive setup picker
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
	{Name: "gmail-mcp", Repo: "Cassandras-Edge/cassandra-gmail-mcp", Description: "Gmail — read inbox, threads, attachments"},
	{Name: "yt-mcp", Repo: "Cassandras-Edge/cassandra-media-mcp", Description: "YouTube — transcribe + search videos (GPU-backed)"},
	{Name: "twitter-mcp", Repo: "Cassandras-Edge/twitter-mcp", Description: "Twitter/X — search, threads, your feed (cookie auth)"},
	{Name: "reddit-mcp", Repo: "Cassandras-Edge/reddit-mcp", Description: "Reddit — public posts + comment threads"},
	{Name: "discord-mcp", Repo: "Cassandras-Edge/cassandra-discord-mcp", Description: "Discord — read your servers and DMs (via Matrix bridge)"},
	{Name: "market-research", Repo: "Cassandras-Edge/cassandra-market-research", Description: "Market data — stocks, SEC filings, options, crypto"},

	// ── Opt-in (--with <name> or --with all) ──
	{Name: "claudeai-mcp", Repo: "Cassandras-Edge/cassandra-claudeai-mcp", Optional: true, Description: "claude.ai — your conversations, projects, knowledge docs"},
	{Name: "gemini-mcp", Repo: "Cassandras-Edge/cassandra-gemini-mcp", Optional: true, Description: "Gemini — grounded web search + reasoning"},
	{Name: "perplexity-mcp", Repo: "Cassandras-Edge/cassandra-perplexity-mcp", Optional: true, Description: "Perplexity — Sonar search + finance scrape"},
	{Name: "tradingview-mcp", Repo: "Cassandras-Edge/cassandra-tradingview-mcp", Optional: true, Description: "TradingView — symbols, news, watchlists, OHLC"},
	{Name: "schwab-mcp", Repo: "Cassandras-Edge/cassandra-schwab-mcp", Optional: true, Description: "Charles Schwab — account positions + orders (read)"},
	{Name: "routines", Repo: "Cassandras-Edge/cassandra-routines", Optional: true, Description: "Routines — manage your scheduled Claude jobs"},
	{Name: "flock-gateway", Repo: "Cassandras-Edge/cassandra-flock-gateway", Optional: true, Description: "flock — manage your codex automations (list/fire runs, read transcripts)"},
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
