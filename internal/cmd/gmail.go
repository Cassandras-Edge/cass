package cmd

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/Cassandras-Edge/cass/internal/auth"
	"github.com/Cassandras-Edge/cass/internal/config"
)

// Google OAuth client for cassandra-gmail-mcp. Set at build time via ldflags
// from env/gmail.env; can be overridden at runtime by CASS_GMAIL_CLIENT_ID /
// CASS_GMAIL_CLIENT_SECRET env vars. For a Desktop-type OAuth client, the
// secret is by-design not confidential (per Google's docs) — it identifies
// the OAuth app for rate-limiting but does not grant access.
var (
	gmailClientID     = ""
	gmailClientSecret = ""
)

const (
	gmailAuthEndpoint   = "https://accounts.google.com/o/oauth2/v2/auth"
	gmailTokenEndpoint  = "https://oauth2.googleapis.com/token"
	gmailRevokeEndpoint = "https://oauth2.googleapis.com/revoke"
	gmailScope          = "https://www.googleapis.com/auth/gmail.readonly"
	gmailAuthService    = "gmail-mcp"
)

func gmailCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gmail",
		Short: "Link your Google account for the gmail-mcp service",
		Long: "Manage the Gmail OAuth refresh token stored in cassandra-auth.\n" +
			"Uses a loopback OAuth flow with PKCE — your refresh token is\n" +
			"posted directly from this machine to cassandra-auth and never\n" +
			"leaves your control. Read-only scope (gmail.readonly) only.",
	}
	cmd.AddCommand(gmailLinkCmd())
	cmd.AddCommand(gmailStatusCmd())
	return cmd
}

func gmailLinkCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "link",
		Short: "Open Google consent screen and store your refresh token in cassandra-auth",
		RunE: func(_ *cobra.Command, _ []string) error {
			clientID, clientSecret, err := loadGmailOAuthClient()
			if err != nil {
				return err
			}
			return runGmailLink(clientID, clientSecret)
		},
	}
}

func gmailStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether Gmail is linked",
		RunE: func(_ *cobra.Command, _ []string) error {
			return runGmailStatus()
		},
	}
}

func loadGmailOAuthClient() (string, string, error) {
	id := os.Getenv("CASS_GMAIL_CLIENT_ID")
	if id == "" {
		id = gmailClientID
	}
	secret := os.Getenv("CASS_GMAIL_CLIENT_SECRET")
	if secret == "" {
		secret = gmailClientSecret
	}
	if id == "" || secret == "" {
		return "", "", errors.New("Google OAuth client not configured. " +
			"Set CASS_GMAIL_CLIENT_ID and CASS_GMAIL_CLIENT_SECRET, " +
			"or rebuild cass with these injected via ldflags " +
			"(scripts/build.sh picks them up from env vars).")
	}
	return id, secret, nil
}

func runGmailLink(clientID, clientSecret string) error {
	verifier, err := randomURLSafe(48)
	if err != nil {
		return err
	}
	challenge := pkceChallengeS256(verifier)
	state, err := randomURLSafe(24)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("loopback listen: %w", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("response_type", "code")
	q.Set("scope", gmailScope)
	q.Set("access_type", "offline")
	q.Set("prompt", "consent") // force refresh_token issuance every time
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	consentURL := gmailAuthEndpoint + "?" + q.Encode()

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		qs := r.URL.Query()
		if qs.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			errCh <- errors.New("state mismatch in OAuth callback (possible CSRF)")
			return
		}
		if e := qs.Get("error"); e != "" {
			http.Error(w, e, http.StatusBadRequest)
			errCh <- fmt.Errorf("google returned error: %s", e)
			return
		}
		code := qs.Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			errCh <- errors.New("missing code in OAuth callback")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, gmailSuccessHTML)
		codeCh <- code
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	fmt.Println("Opening browser for Google consent...")
	fmt.Println("If it doesn't open automatically, paste this URL:")
	fmt.Println("  " + consentURL)
	openBrowser(consentURL)

	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		return err
	case <-time.After(5 * time.Minute):
		return errors.New("timed out waiting for Google consent")
	}

	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("grant_type", "authorization_code")
	form.Set("code_verifier", verifier)
	resp, err := http.PostForm(gmailTokenEndpoint, form)
	if err != nil {
		return fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode != 200 {
		return fmt.Errorf("token exchange %s: %s", resp.Status, string(body))
	}
	var tok struct {
		RefreshToken string `json:"refresh_token"`
		AccessToken  string `json:"access_token"`
		Scope        string `json:"scope"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return fmt.Errorf("decode token response: %w", err)
	}
	if tok.RefreshToken == "" {
		return errors.New("Google did not return a refresh_token. " +
			"This usually means the account was already consented and the " +
			"refresh token was issued previously. Revoke at " +
			"https://myaccount.google.com/permissions and run `cass gmail link` again.")
	}

	fmt.Println("Pushing refresh token to cassandra-auth...")
	if err := pushCredentials(gmailAuthService, map[string]string{
		"refresh_token": tok.RefreshToken,
		"scope":         tok.Scope,
		"linked_at":     time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		return err
	}
	fmt.Println("Gmail linked. gmail-mcp will use this token to call the Gmail API on your behalf.")
	return nil
}

func runGmailStatus() error {
	creds, err := auth.Read()
	if err != nil {
		return fmt.Errorf("not logged in (run: cass login): %w", err)
	}
	u := fmt.Sprintf("%s/api/extension/credentials/%s", config.PortalURL(), gmailAuthService)
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("CF-Access-Client-Id", creds.CFAccessClientID)
	req.Header.Set("CF-Access-Client-Secret", creds.CFAccessClientSecret)
	req.Header.Set("Authorization", "Bearer "+creds.MCPKey)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNoContent {
		fmt.Println("Gmail: NOT LINKED")
		fmt.Println("  Run: cass gmail link")
		return nil
	}
	if resp.StatusCode != 200 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("portal %s: %s", resp.Status, string(buf))
	}
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return err
	}
	// Portal route returns the credentials object directly (or {} if absent).
	// Some older code paths wrap it under {"credentials": {...}} — handle both.
	if inner, ok := raw["credentials"].(map[string]any); ok {
		raw = inner
	}
	if raw == nil || raw["refresh_token"] == nil {
		fmt.Println("Gmail: NOT LINKED")
		fmt.Println("  Run: cass gmail link")
		return nil
	}
	fmt.Println("Gmail: LINKED")
	if s, ok := raw["linked_at"].(string); ok {
		fmt.Printf("  linked at: %s\n", s)
	}
	if s, ok := raw["scope"].(string); ok {
		fmt.Printf("  scope:     %s\n", s)
	}
	return nil
}

func pkceChallengeS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func randomURLSafe(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

const gmailSuccessHTML = `<!doctype html>
<html><head><meta charset="utf-8"><title>Gmail linked</title>
<style>body{font:16px/1.5 -apple-system,BlinkMacSystemFont,sans-serif;
max-width:480px;margin:80px auto;padding:0 20px;color:#222}
h1{font-size:20px;margin:0 0 8px}p{color:#666;margin:8px 0}</style></head>
<body><h1>Gmail linked.</h1>
<p>You can close this tab and return to your terminal.</p></body></html>`
