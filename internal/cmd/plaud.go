package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"

	"github.com/Cassandras-Edge/cass/internal/auth"
	"github.com/Cassandras-Edge/cass/internal/config"
)

// Plaud auth is NOT OAuth — it's a phone + SMS one-time-code flow against the
// consumer API at api.plaud.ai. Two calls:
//
//	POST /auth/sms/code {phone_code, phone_number}        -> sends SMS, returns a `token`
//	POST /auth/login    {phone_code, phone_number, code, token} -> returns the bearer JWT
//
// The JWT (~300-day life) is stored per-user in cassandra-auth under
// credentials/{email}/plaud-mcp = {plaud_token, plaud_region} and is what
// cassandra-plaud-mcp uses to mirror your recordings. `plaud_region` is left
// empty here — the backend client auto-detects on a -302 region mismatch.
const (
	plaudAPIBase     = "https://api.plaud.ai"
	plaudAuthService = "plaud-mcp"
)

var plaudBrowserHeaders = map[string]string{
	"Accept":       "*/*",
	"Origin":       "https://web.plaud.ai",
	"Referer":      "https://web.plaud.ai/",
	"Content-Type": "application/json",
	"User-Agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
		"AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.6 Safari/605.1.15",
	"app-platform": "web",
	"edit-from":    "web",
}

func plaudCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plaud",
		Short: "Link your Plaud account for the plaud-mcp service",
		Long: "Capture your Plaud session JWT and store it in cassandra-auth.\n" +
			"Plaud uses phone + SMS one-time-code login (not OAuth): you enter\n" +
			"your phone number, Plaud texts a code, and the resulting bearer\n" +
			"token (~300-day life) is pushed straight from this machine to\n" +
			"cassandra-auth. The token never leaves your control.",
	}
	cmd.AddCommand(plaudLoginCmd())
	cmd.AddCommand(plaudStatusCmd())
	return cmd
}

func plaudLoginCmd() *cobra.Command {
	var pasteToken string
	c := &cobra.Command{
		Use:   "login",
		Short: "Log in to Plaud (SMS code) and store the token in cassandra-auth",
		RunE: func(_ *cobra.Command, _ []string) error {
			if pasteToken != "" {
				return storePlaudToken(strings.TrimSpace(pasteToken))
			}
			return runPlaudLogin()
		},
	}
	c.Flags().StringVar(&pasteToken, "token", "", "skip SMS login and store a bearer JWT you already have "+
		"(escape hatch for SSO-only accounts — grab it from web.plaud.ai devtools)")
	return c
}

func plaudStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether Plaud is linked",
		RunE: func(_ *cobra.Command, _ []string) error {
			return runPlaudStatus()
		},
	}
}

func runPlaudLogin() error {
	var phoneCode, phoneNumber string
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Country calling code").
				Description("digits only, no '+' (e.g. 1 for US, 44 for UK)").
				Value(&phoneCode).
				Validate(func(s string) error {
					s = strings.TrimSpace(strings.TrimPrefix(s, "+"))
					if s == "" {
						return errors.New("required")
					}
					return nil
				}),
			huh.NewInput().
				Title("Phone number").
				Description("the number on your Plaud account, no spaces").
				Value(&phoneNumber).
				Validate(func(s string) error {
					if strings.TrimSpace(s) == "" {
						return errors.New("required")
					}
					return nil
				}),
		),
	)
	if err := form.Run(); err != nil {
		return err
	}
	phoneCode = strings.TrimSpace(strings.TrimPrefix(phoneCode, "+"))
	phoneNumber = strings.TrimSpace(phoneNumber)

	// Step 1: trigger the SMS, capture the verification token.
	fmt.Println("Requesting SMS code from Plaud...")
	smsResp, err := plaudPost("/auth/sms/code", map[string]any{
		"phone_code":   phoneCode,
		"phone_number": phoneNumber,
	})
	if err != nil {
		return fmt.Errorf("send SMS code: %w", err)
	}
	verifyToken := firstString(smsResp, "token", "verify_token", "captcha_token")
	if verifyToken == "" {
		// Some tenants don't return a session token from this step; /auth/login
		// may still accept an empty one. Warn but continue.
		fmt.Println("  (no verification token returned — continuing)")
	}

	// Step 2: read the code the user just received, exchange for the JWT.
	var code string
	codeForm := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("SMS code").
				Description("the code Plaud just texted to " + phoneCode + " " + phoneNumber).
				Value(&code).
				Validate(func(s string) error {
					if strings.TrimSpace(s) == "" {
						return errors.New("required")
					}
					return nil
				}),
		),
	)
	if err := codeForm.Run(); err != nil {
		return err
	}
	code = strings.TrimSpace(code)

	fmt.Println("Exchanging code for token...")
	loginResp, err := plaudPost("/auth/login", map[string]any{
		"phone_code":   phoneCode,
		"phone_number": phoneNumber,
		"code":         code,
		"token":        verifyToken,
	})
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	jwt := extractPlaudJWT(loginResp)
	if jwt == "" {
		return fmt.Errorf("login succeeded but no token found in response: %s", jsonSnippet(loginResp))
	}
	return storePlaudToken(jwt)
}

// extractPlaudJWT pulls the bearer JWT out of the /auth/login response,
// tolerating envelope variation ({data:{...}} vs flat) and field naming.
func extractPlaudJWT(resp map[string]any) string {
	if t := firstString(resp, "access_token", "token", "jwt", "id_token", "auth_token"); t != "" {
		return t
	}
	if data, ok := resp["data"].(map[string]any); ok {
		return firstString(data, "access_token", "token", "jwt", "id_token", "auth_token")
	}
	return ""
}

func storePlaudToken(token string) error {
	if !strings.HasPrefix(token, "eyJ") {
		fmt.Println("Warning: token doesn't look like a JWT (expected to start with 'eyJ').")
	}
	fmt.Println("Pushing Plaud token to cassandra-auth...")
	if err := pushCredentials(plaudAuthService, map[string]string{
		"plaud_token":  token,
		"plaud_region": "", // backend auto-detects region on -302 mismatch
		"linked_at":    time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		return err
	}
	fmt.Println("Plaud linked. plaud-mcp will mirror your recordings with this token.")
	return nil
}

func runPlaudStatus() error {
	creds, err := auth.Read()
	if err != nil {
		return fmt.Errorf("not logged in (run: cass login): %w", err)
	}
	u := fmt.Sprintf("%s/api/extension/credentials/%s", config.PortalURL(), plaudAuthService)
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
		fmt.Println("Plaud: NOT LINKED")
		fmt.Println("  Run: cass plaud login")
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
	if inner, ok := raw["credentials"].(map[string]any); ok {
		raw = inner
	}
	if raw == nil || raw["plaud_token"] == nil {
		fmt.Println("Plaud: NOT LINKED")
		fmt.Println("  Run: cass plaud login")
		return nil
	}
	fmt.Println("Plaud: LINKED")
	if s, ok := raw["linked_at"].(string); ok {
		fmt.Printf("  linked at: %s\n", s)
	}
	if s, ok := raw["plaud_region"].(string); ok && s != "" {
		fmt.Printf("  region:    %s\n", s)
	}
	return nil
}

// plaudPost issues a browser-shaped POST and decodes the JSON envelope. It
// surfaces Plaud's negative-status envelope ({status:-N, msg:...}) as an error.
func plaudPost(path string, body map[string]any) (map[string]any, error) {
	buf, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", plaudAPIBase+path, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	for k, v := range plaudBrowserHeaders {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("%s: non-JSON response (%s): %s", path, resp.Status, snippet(raw))
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s %s: %s", path, resp.Status, plaudErrMsg(data, raw))
	}
	if st, ok := data["status"].(float64); ok && st < 0 {
		return nil, fmt.Errorf("%s status %d: %s", path, int(st), plaudErrMsg(data, raw))
	}
	return data, nil
}

func plaudErrMsg(data map[string]any, raw []byte) string {
	if m := firstString(data, "msg", "message", "detail"); m != "" {
		return m
	}
	return snippet(raw)
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func snippet(raw []byte) string {
	if len(raw) > 300 {
		return string(raw[:300]) + "…"
	}
	return string(raw)
}

func jsonSnippet(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return snippet(b)
}
