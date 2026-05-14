package config

import "os"

const (
	DefaultPortalURL     = "https://portal.cassandrasedge.com"
	DefaultAuthURL       = "https://auth.cassandrasedge.com"
	DefaultShareURL      = "https://share.cassandrasedge.com"
	DefaultTwitterMcpURL = "https://twitter-mcp.cassandrasedge.com"
)

func PortalURL() string {
	if u := os.Getenv("CASS_PORTAL_URL"); u != "" {
		return u
	}
	return DefaultPortalURL
}

func AuthURL() string {
	if u := os.Getenv("AUTH_URL"); u != "" {
		return u
	}
	return DefaultAuthURL
}

// AuthSecret is the shared admin secret for direct auth-service writes.
// Returns "" when not set — caller decides whether the command is admin-only.
func AuthSecret() string {
	return os.Getenv("AUTH_SECRET")
}

// TwitterMcpURL is the public base URL for the twitter-mcp service.
// Used by `cass twitter sync-queryids` to push scraped queryIds via the
// service's /admin/queryids endpoint (no auth port-forward needed).
func TwitterMcpURL() string {
	if u := os.Getenv("TWITTER_MCP_URL"); u != "" {
		return u
	}
	return DefaultTwitterMcpURL
}
