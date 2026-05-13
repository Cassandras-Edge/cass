package share

import "regexp"

// Well-known credential patterns. Caught before upload — first defense.
var secretPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`sk-[A-Za-z0-9_\-]{20,}`), "<OPENAI_KEY>"},
	{regexp.MustCompile(`ghp_[A-Za-z0-9]{30,}`), "<GITHUB_TOKEN>"},
	{regexp.MustCompile(`AKIA[0-9A-Z]{16}`), "<AWS_ACCESS_KEY_ID>"},
	{regexp.MustCompile(`AIza[0-9A-Za-z_\-]{35}`), "<GOOGLE_API_KEY>"},
	{regexp.MustCompile(`sk_live_[A-Za-z0-9]{20,}`), "<STRIPE_KEY>"},
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z ]+PRIVATE KEY-----.+?-----END [A-Z ]+PRIVATE KEY-----`), "<PRIVATE_KEY_PEM>"},
	{regexp.MustCompile(`eyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+`), "<JWT>"},
}

var (
	pathPattern     = regexp.MustCompile(`/Users/[a-zA-Z0-9._-]+`)
	hostnamePattern = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
)

// Sanitize replaces well-known credential formats, absolute home paths, and
// private IPs. Local-only — no network. The share service runs its own
// deeper scan server-side.
func Sanitize(text string) string {
	for _, p := range secretPatterns {
		text = p.re.ReplaceAllString(text, p.repl)
	}
	text = pathPattern.ReplaceAllString(text, "<HOME>")
	text = hostnamePattern.ReplaceAllString(text, "<INTERNAL_IP>")
	return text
}
