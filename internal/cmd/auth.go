package cmd

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Cassandras-Edge/cass/internal/portal"
)

const (
	schwabTokenURL = "https://api.schwabapi.com/v1/oauth/token"
	schwabAuthURL  = "https://api.schwabapi.com/v1/oauth/authorize"
)

var reauthStates = map[string]bool{
	"disabled":        true,
	"reauth_required": true,
}

func authSchwabCmd() *cobra.Command {
	var sessionID string
	cmd := &cobra.Command{
		Use:   "schwab",
		Short: "Authenticate Schwab via OAuth and upload the token to portal",
		Long: `Schwab OAuth uses the manual flow:

1. cass bootstraps a connect session against the portal (gets app_key,
   app_secret, callback_url).
2. cass prints the Schwab authorization URL — open it in a browser.
3. After authorizing, Schwab redirects to a URL that will fail to load.
   Copy the FULL failed URL from your browser's address bar and paste it back.
4. cass exchanges the embedded code for tokens and posts them to portal.

(The Python --local browser-loopback mode isn't ported. Use this manual
flow — it's the same thing schwab-py does in --manual mode.)`,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runAuthSchwab(sessionID)
		},
	}
	cmd.Flags().StringVar(&sessionID, "session-id", "", "Existing portal connect session id")
	return cmd
}

type schwabBootstrap struct {
	SessionID   string `json:"session_id"`
	CallbackURL string `json:"callback_url"`
	AppKey      string `json:"app_key"`
	AppSecret   string `json:"app_secret"`
}

type schwabStatus struct {
	State   string `json:"state"`
	Message string `json:"message"`
}

func runAuthSchwab(sessionID string) error {
	c, err := portal.NewClient()
	if err != nil {
		return err
	}

	path := "/api/schwab/bootstrap"
	if sessionID != "" {
		path += "?session_id=" + url.QueryEscape(sessionID)
	}
	var boot schwabBootstrap
	if err := c.Post(path, struct{}{}, &boot); err != nil {
		return fmt.Errorf("portal bootstrap: %w", err)
	}
	for _, pair := range []struct{ name, val string }{
		{"session_id", boot.SessionID},
		{"callback_url", boot.CallbackURL},
		{"app_key", boot.AppKey},
		{"app_secret", boot.AppSecret},
	} {
		if pair.val == "" {
			return fmt.Errorf("portal bootstrap returned incomplete response (missing %s)", pair.name)
		}
	}

	fmt.Printf("Schwab connect session: %s\n", boot.SessionID)
	fmt.Printf("Callback URL:           %s\n", boot.CallbackURL)
	fmt.Println()

	authURL := schwabAuthURL +
		"?client_id=" + url.QueryEscape(boot.AppKey) +
		"&redirect_uri=" + url.QueryEscape(boot.CallbackURL)
	fmt.Println("Open this URL in your browser and authorize the app:")
	fmt.Println()
	fmt.Println("  " + authURL)
	fmt.Println()
	fmt.Println("After authorizing, Schwab will redirect to the callback URL (which will fail to load).")
	fmt.Println("Copy the FULL URL from your browser's address bar and paste it below.")
	fmt.Println()
	fmt.Print("Paste redirect URL: ")

	reader := bufio.NewReader(os.Stdin)
	raw, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read redirect URL: %w", err)
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("no URL pasted")
	}
	code, err := extractSchwabCode(raw)
	if err != nil {
		return err
	}

	token, err := exchangeSchwabCode(boot.AppKey, boot.AppSecret, boot.CallbackURL, code)
	if err != nil {
		return fmt.Errorf("exchange code for token: %w", err)
	}

	// schwab-py's on-disk format. The portal's /complete endpoint accepts the
	// outer envelope verbatim.
	tokenPayload := map[string]any{
		"creation_timestamp": time.Now().Unix(),
		"token":              token,
	}

	if err := c.Post("/api/schwab/connect/complete/"+boot.SessionID,
		map[string]any{"token": tokenPayload}, nil); err != nil {
		return fmt.Errorf("post token to portal: %w", err)
	}

	var status schwabStatus
	if err := c.Get("/api/schwab/status", &status); err != nil {
		return fmt.Errorf("read schwab status: %w", err)
	}
	fmt.Printf("\nCurrent state: %s\n", status.State)
	if status.Message != "" {
		fmt.Println(status.Message)
	}
	return nil
}

// extractSchwabCode pulls the `code` query param out of the URL the user
// pasted. The redirect URL looks like:
//
//	https://callback.example.com/?code=ABC123&session=...
//
// Schwab URL-encodes everything; we URL-decode the value before returning.
func extractSchwabCode(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("paste is not a valid URL: %w", err)
	}
	code := u.Query().Get("code")
	if code == "" {
		return "", fmt.Errorf("paste did not contain a `code` query parameter")
	}
	return code, nil
}

func exchangeSchwabCode(appKey, appSecret, callbackURL, code string) (map[string]any, error) {
	form := url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {callbackURL},
	}
	req, err := http.NewRequest("POST", schwabTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	basic := base64.StdEncoding.EncodeToString([]byte(appKey + ":" + appSecret))
	req.Header.Set("Authorization", "Basic "+basic)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		snippet := string(body)
		if len(snippet) > 400 {
			snippet = snippet[:400]
		}
		return nil, fmt.Errorf("Schwab %s: %s", resp.Status, snippet)
	}
	var token map[string]any
	if err := json.Unmarshal(body, &token); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	if exp, ok := token["expires_in"].(float64); ok {
		token["expires_at"] = time.Now().Unix() + int64(exp)
	}
	return token, nil
}

// ─── auth status ───────────────────────────────────────────────────────────

func authStatusCmd() *cobra.Command {
	var services []string
	var ifNeeded, quiet bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Check upstream-service auth state — exits non-zero if anything needs attention",
		Long: `Designed for a Claude Code SessionStart hook:

  { "hooks": { "SessionStart": [{ "hooks": [
      { "type": "command",
        "command": "cass auth status --if-needed --quiet" }
  ] }] } }`,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runAuthStatus(services, ifNeeded, quiet)
		},
	}
	cmd.Flags().StringSliceVar(&services, "service", nil, "Limit to specific services (default: all)")
	cmd.Flags().BoolVar(&ifNeeded, "if-needed", false, "Run the re-auth flow inline when a service is not healthy")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "No output on healthy sessions — only print on problems")
	return cmd
}

func runAuthStatus(services []string, ifNeeded, quiet bool) error {
	selected := map[string]bool{}
	for _, s := range services {
		selected[strings.ToLower(s)] = true
	}
	check := func(name string) bool {
		return len(selected) == 0 || selected[name]
	}

	if check("schwab") {
		status := fetchSchwabStatus()
		if status == nil {
			if !quiet {
				fmt.Println("schwab: unknown (portal unreachable or not logged in)")
			}
			os.Exit(1)
		}
		switch {
		case status.State == "healthy":
			if !quiet {
				fmt.Printf("schwab: healthy — %s\n", status.Message)
			}
		case reauthStates[status.State]:
			fmt.Fprintf(os.Stderr, "schwab: %s — %s\n", status.State, status.Message)
			if ifNeeded {
				fmt.Fprintln(os.Stderr, "Running `cass auth schwab`…")
				return runAuthSchwab("")
			}
			os.Exit(1)
		default:
			fmt.Fprintf(os.Stderr, "schwab: %s — %s\n", status.State, status.Message)
			os.Exit(1)
		}
	}
	return nil
}

func fetchSchwabStatus() *schwabStatus {
	c, err := portal.NewClient()
	if err != nil {
		return nil
	}
	var status schwabStatus
	if err := c.Get("/api/schwab/status", &status); err != nil {
		return nil
	}
	return &status
}
