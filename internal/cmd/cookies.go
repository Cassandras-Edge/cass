package cmd

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Cassandras-Edge/cass/internal/auth"
	"github.com/Cassandras-Edge/cass/internal/browser"
	"github.com/Cassandras-Edge/cass/internal/config"
)

type cookieService struct {
	Name           string
	CredentialKey  string            // For "full jar" mode (single base64 blob)
	CookieNames    map[string]string // For "named" mode (jar-name → credential-name)
	Domains        []string
	LoginURL       string
	ProbeURL       string
	Description    string
	RequiredCookie []string // For skip_validate services — sanity check before push
	SkipValidate   bool
}

// cookieServices is the static catalog mirrored from cookies.py SERVICES.
var cookieServices = map[string]*cookieService{
	"yt-mcp": {
		Name:          "yt-mcp",
		CredentialKey: "youtube_cookies",
		Domains:       []string{".youtube.com", ".google.com"},
		LoginURL:      "https://accounts.google.com/ServiceLogin?service=youtube",
		ProbeURL:      "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		Description:   "YouTube cookies for yt-dlp",
	},
	"twitter": {
		Name:          "twitter",
		CredentialKey: "twitter_cookies",
		CookieNames:   map[string]string{"auth_token": "twitter_auth_token", "ct0": "twitter_ct0"},
		Domains:       []string{".x.com", ".twitter.com"},
		LoginURL:      "https://x.com/i/flow/login",
		ProbeURL:      "https://x.com",
		Description:   "Twitter/X auth cookies",
	},
	"claude-ai": {
		Name:           "claude-ai",
		CredentialKey:  "claude_cookies",
		Domains:        []string{".claude.ai", "claude.ai"},
		LoginURL:       "https://claude.ai/login",
		ProbeURL:       "https://claude.ai",
		Description:    "Claude.ai session cookies",
		RequiredCookie: []string{"sessionKey"},
		SkipValidate:   true, // yt-dlp can't fetch claude.ai (not a media site)
	},
	"perplexity-mcp": {
		Name:           "perplexity-mcp",
		CredentialKey:  "perplexity_cookies",
		Domains:        []string{".perplexity.ai", "www.perplexity.ai"},
		LoginURL:       "https://www.perplexity.ai/finance/AAPL",
		ProbeURL:       "https://www.perplexity.ai/finance/AAPL",
		Description:    "Perplexity Finance cookies (cf_clearance + session)",
		RequiredCookie: []string{"cf_clearance"},
		SkipValidate:   true,
	},
}

func cookiesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cookies",
		Short: "Sync browser cookies to platform services",
	}
	cmd.AddCommand(cookiesSyncCmd())
	cmd.AddCommand(cookiesStatusCmd())
	cmd.AddCommand(cookiesTestCmd())
	cmd.AddCommand(cookiesRefreshCmd())
	return cmd
}

func cookiesSyncCmd() *cobra.Command {
	var dryRun, noOpen bool
	cmd := &cobra.Command{
		Use:   "sync [services...]",
		Short: "Extract Firefox cookies for one or more services and push them to auth",
		RunE: func(_ *cobra.Command, args []string) error {
			targets := args
			if len(targets) == 0 {
				for k := range cookieServices {
					targets = append(targets, k)
				}
				sort.Strings(targets)
			}
			for _, name := range targets {
				svc, ok := cookieServices[name]
				if !ok {
					known := serviceNames()
					fmt.Printf("Unknown service: %s (available: %s)\n", name, strings.Join(known, ", "))
					continue
				}
				fmt.Printf("\n── %s: %s ──\n", svc.Name, svc.Description)
				syncOneService(svc, dryRun, noOpen)
			}
			fmt.Println()
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Extract cookies but don't push")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "Don't open login pages on missing cookies (for unattended use)")
	return cmd
}

func cookiesStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show which services have Firefox cookies present (fast, no network)",
		RunE: func(_ *cobra.Command, _ []string) error {
			// Fast path: read Firefox's moz_cookies SQLite directly. ~5ms/service
			// vs ~1-2s/service for yt-dlp shell-out. Matches Python's check.
			missing := []string{}
			for _, name := range serviceNames() {
				svc := cookieServices[name]
				var required []string
				if svc.CookieNames != nil {
					for k := range svc.CookieNames {
						required = append(required, k)
					}
				}
				ok := browser.HasFirefoxCookies(svc.Domains, required)
				icon := "ok"
				if !ok {
					icon = "MISSING"
					missing = append(missing, name)
				}
				fmt.Printf("  %-14s %s\n", name, icon)
			}
			if len(missing) > 0 {
				fmt.Printf("\nRun: cass cookies sync %s\n", strings.Join(missing, " "))
				os.Exit(1)
			}
			return nil
		},
	}
}

func cookiesTestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "test",
		Short: "Verify that yt-dlp can fetch a YouTube page with Firefox cookies",
		RunE: func(_ *cobra.Command, _ []string) error {
			if _, err := exec.LookPath("yt-dlp"); err != nil {
				return fmt.Errorf("yt-dlp not found in PATH")
			}
			fmt.Println("Testing yt-dlp with firefox cookies...")
			out, err := exec.Command(
				"yt-dlp", "--cookies-from-browser", "firefox", "--skip-download",
				"--print", "title", "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
			).CombinedOutput()
			if err != nil {
				return fmt.Errorf("FAIL: %s", strings.TrimSpace(string(out)))
			}
			fmt.Printf("OK — got title: %s\n", strings.TrimSpace(string(out)))
			return nil
		},
	}
}

func cookiesRefreshCmd() *cobra.Command {
	var timeoutSec int
	var force bool
	cmd := &cobra.Command{
		Use:   "refresh [services...]",
		Short: "Background-refresh Cloudflare-gated cookies in Firefox, then sync",
		Long: `Currently meaningful only for services with Cloudflare bot protection
(perplexity-mcp). Snapshots cf_clearance from Firefox's SQLite, opens
the probe URL in a background tab, then polls until cf_clearance
rotates (or --timeout elapses). Matches the Python behavior.`,
		RunE: func(_ *cobra.Command, args []string) error {
			targets := args
			if len(targets) == 0 {
				targets = []string{"perplexity-mcp"}
			}
			for _, name := range targets {
				svc, ok := cookieServices[name]
				if !ok {
					fmt.Printf("Unknown service: %s\n", name)
					continue
				}
				fmt.Printf("\n── refresh %s ──\n", name)
				refreshOneService(svc, timeoutSec, force)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&timeoutSec, "timeout", 30, "Seconds to wait for Firefox to refresh Cloudflare cookies")
	cmd.Flags().BoolVar(&force, "force", false, "Open Firefox even if cookies look fresh")
	return cmd
}

func refreshOneService(svc *cookieService, timeoutSec int, force bool) {
	// cf_clearance lives on perplexity.ai's host. For other services that
	// don't ride Cloudflare bot protection, the refresh is just a sync.
	var clearanceHost string
	if svc.Name == "perplexity-mcp" {
		clearanceHost = "%perplexity.ai"
	}

	var beforeValue string
	var beforeExpiry int64
	if clearanceHost != "" {
		beforeValue, beforeExpiry = browser.ReadFirefoxCookie(clearanceHost, "cf_clearance")
		if !force && beforeValue != "" {
			ttl := beforeExpiry - time.Now().Unix()
			if ttl > 3600 {
				fmt.Printf("  cf_clearance still has %d min left — running sync.\n", ttl/60)
				syncOneService(svc, false, true)
				return
			}
		}
	}

	fmt.Printf("  Opening %s in background Firefox...\n", svc.ProbeURL)
	_ = exec.Command("open", "-a", "Firefox", "-g", svc.ProbeURL).Run()

	if clearanceHost != "" {
		deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
		refreshed := false
		for time.Now().Before(deadline) {
			time.Sleep(2 * time.Second)
			nowVal, _ := browser.ReadFirefoxCookie(clearanceHost, "cf_clearance")
			if nowVal != "" && nowVal != beforeValue {
				refreshed = true
				break
			}
		}
		if refreshed {
			fmt.Println("  cf_clearance refreshed.")
		} else {
			fmt.Fprintf(os.Stderr, "  WARN: no cf_clearance change in %ds — syncing what we have.\n", timeoutSec)
		}
	} else {
		// No Cloudflare gate — small delay then sync.
		time.Sleep(2 * time.Second)
	}
	syncOneService(svc, false, true)
}

func syncOneService(svc *cookieService, dryRun, noOpen bool) {
	promptLogin := func(msg string) {
		fmt.Printf("  %s\n", msg)
		if noOpen {
			fmt.Printf("  Sign in to Firefox, then re-run: cass cookies sync %s\n", svc.Name)
		} else {
			_ = exec.Command("open", "-a", "Firefox", svc.LoginURL).Run()
			fmt.Println("  Sign in, then re-run this command.")
		}
	}
	fmt.Println("  Extracting from firefox...")
	cookies, err := browser.ReadFirefoxCookies(svc.Domains)
	if err != nil || len(cookies) == 0 {
		promptLogin("No cookies found.")
		return
	}

	var creds map[string]string
	if svc.CookieNames != nil {
		creds = map[string]string{}
		for _, c := range cookies {
			if mapped, ok := svc.CookieNames[c.Name]; ok {
				creds[mapped] = c.Value
			}
		}
		if len(creds) == 0 {
			promptLogin("Cookies present but missing required keys.")
			return
		}
	} else {
		lines := make([]string, 0, len(cookies))
		for _, c := range cookies {
			lines = append(lines, c.NetscapeLine())
		}
		jar := "# Netscape HTTP Cookie File\n" + strings.Join(lines, "\n") + "\n"
		creds = map[string]string{svc.CredentialKey: base64.StdEncoding.EncodeToString([]byte(jar))}
	}

	if !svc.SkipValidate && svc.CredentialKey != "" && creds[svc.CredentialKey] != "" {
		if _, err := exec.LookPath("yt-dlp"); err != nil {
			fmt.Println("  (yt-dlp not installed — skipping fetch validation)")
		} else {
			fmt.Println("  Validating cookies...")
			ok, detail := validateCookiesB64(creds[svc.CredentialKey], svc.ProbeURL)
			if ok {
				fmt.Printf("  Valid — %s\n", detail)
			} else {
				fmt.Fprintf(os.Stderr, "  INVALID — %s\n", detail)
				promptLogin("Cookies are stale or logged out.")
				return
			}
		}
	}

	if svc.SkipValidate && len(svc.RequiredCookie) > 0 {
		jarB64 := creds[svc.CredentialKey]
		raw, _ := base64.StdEncoding.DecodeString(jarB64)
		missing := []string{}
		for _, n := range svc.RequiredCookie {
			if !strings.Contains(string(raw), "\t"+n+"\t") {
				missing = append(missing, n)
			}
		}
		if len(missing) > 0 {
			fmt.Fprintf(os.Stderr, "  MISSING required cookies: %s\n", strings.Join(missing, ", "))
			promptLogin(fmt.Sprintf("Visit %s in Firefox to get fresh cookies.", svc.ProbeURL))
			return
		}
	}

	if dryRun {
		keys := make([]string, 0, len(creds))
		for k := range creds {
			keys = append(keys, k)
		}
		fmt.Printf("  Found: %s\n", strings.Join(keys, ", "))
		fmt.Println("  Dry run — not pushing.")
		return
	}
	if err := pushCredentials(svc.Name, creds); err != nil {
		fmt.Fprintf(os.Stderr, "  push failed: %v\n", err)
		return
	}
	keys := make([]string, 0, len(creds))
	for k := range creds {
		keys = append(keys, k)
	}
	fmt.Printf("  Synced: %s ✓\n", strings.Join(keys, ", "))
}

func validateCookiesB64(jarB64, probeURL string) (bool, string) {
	if _, err := exec.LookPath("yt-dlp"); err != nil {
		return false, "yt-dlp not found"
	}
	tmp, err := os.MkdirTemp("", "cass-validate-")
	if err != nil {
		return false, err.Error()
	}
	defer os.RemoveAll(tmp)
	cookiesPath := filepath.Join(tmp, "cookies.txt")
	raw, err := base64.StdEncoding.DecodeString(jarB64)
	if err != nil {
		return false, "decode jar: " + err.Error()
	}
	if err := os.WriteFile(cookiesPath, raw, 0o600); err != nil {
		return false, err.Error()
	}
	out, err := exec.Command(
		"yt-dlp", "--cookies", cookiesPath, "--skip-download",
		"--print", "title", "--no-warnings", probeURL,
	).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		s := strings.TrimSpace(string(out))
		if len(s) > 200 {
			s = s[:200]
		}
		if s == "" {
			s = "yt-dlp failed with no output"
		}
		return false, s
	}
	return true, strings.TrimSpace(string(out))
}

func pushCredentials(service string, credentials map[string]string) error {
	creds, err := auth.Read()
	if err != nil {
		return fmt.Errorf("not logged in (run: cass login): %w", err)
	}
	body, _ := json.Marshal(credentials)
	url := fmt.Sprintf("%s/api/extension/credentials/%s", config.PortalURL(), service)
	req, _ := http.NewRequest("PUT", url, bytes.NewReader(body))
	req.Header.Set("CF-Access-Client-Id", creds.CFAccessClientID)
	req.Header.Set("CF-Access-Client-Secret", creds.CFAccessClientSecret)
	req.Header.Set("Authorization", "Bearer "+creds.MCPKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("portal %s: %s", resp.Status, string(buf))
	}
	// Read-back verify — portal route returns the credentials object directly.
	verifyReq, _ := http.NewRequest("GET", url, nil)
	verifyReq.Header.Set("CF-Access-Client-Id", creds.CFAccessClientID)
	verifyReq.Header.Set("CF-Access-Client-Secret", creds.CFAccessClientSecret)
	verifyReq.Header.Set("Authorization", "Bearer "+creds.MCPKey)
	vresp, err := (&http.Client{Timeout: 15 * time.Second}).Do(verifyReq)
	if err != nil {
		return fmt.Errorf("PUT succeeded but verify failed: %w", err)
	}
	defer vresp.Body.Close()
	var stored map[string]any
	if err := json.NewDecoder(vresp.Body).Decode(&stored); err == nil {
		if inner, ok := stored["credentials"].(map[string]any); ok {
			stored = inner
		}
		var missing []string
		for k, v := range credentials {
			if got, _ := stored[k].(string); got != v {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("PUT returned 200 but read-back is missing keys: %s", strings.Join(missing, ", "))
		}
	}
	return nil
}

func serviceNames() []string {
	names := make([]string, 0, len(cookieServices))
	for k := range cookieServices {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
