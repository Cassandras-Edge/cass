package cmd

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"

	"github.com/Cassandras-Edge/cass/internal/auth"
	"github.com/Cassandras-Edge/cass/internal/config"
)

// Plaud auth is NOT OAuth. Two ways in:
//
//  1. BROWSER (default) — if you sign in to web.plaud.ai with Google/Apple SSO,
//     there is no phone number, so the SMS flow below cannot work. Instead we
//     read the live session straight out of Chrome's localStorage (leveldb,
//     plaintext) the same way `cass tradingview`/`cass perplexity` lift cookies.
//     Plaud's web session is a TWO-token model:
//     - workspace_token (the bearer used for /file/* calls) — ~24h life
//     - refresh_token                                       — ~27d HARD ceiling
//     We capture both + the workspaceId; the backend refreshes the 24h token
//     from the refresh_token on every sync (POST /user-app/auth/workspace/
//     refresh/{wid}). After ~27d the refresh token expires for good and you
//     re-run `cass plaud login` to capture a fresh pair.
//
//  2. SMS (`--sms`) — phone + one-time-code against api.plaud.ai. Only works
//     for phone-number accounts:
//     POST /auth/sms/code {phone_code, phone_number}              -> sends SMS
//     POST /auth/login    {phone_code, phone_number, code, token} -> bearer JWT
//
// Either way the result lands in cassandra-auth under
// credentials/{email}/plaud-mcp = {plaud_token, plaud_refresh_token,
// plaud_workspace_id, plaud_region, ...} and drives cassandra-plaud-mcp's
// mirror. `plaud_region` is left empty — the backend auto-detects on a -302.
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
		Long: "Capture your Plaud session and store it in cassandra-auth.\n\n" +
			"By default this reads your live web.plaud.ai session from Chrome's\n" +
			"local storage (workspace token + refresh token) — the right path for\n" +
			"Google/Apple SSO accounts that have no phone number. The backend then\n" +
			"refreshes the short-lived token automatically for ~27 days, after\n" +
			"which you re-run this command.\n\n" +
			"Use --sms for the phone + one-time-code flow (phone-number accounts),\n" +
			"or --token to paste a bearer JWT you already have.",
	}
	cmd.AddCommand(plaudLoginCmd())
	cmd.AddCommand(plaudStatusCmd())
	return cmd
}

func plaudLoginCmd() *cobra.Command {
	var pasteToken string
	var useSMS bool
	var browser string
	var profile string
	c := &cobra.Command{
		Use:   "login",
		Short: "Link Plaud — reads your Chrome session by default (use --sms or --token)",
		RunE: func(_ *cobra.Command, _ []string) error {
			switch {
			case pasteToken != "":
				return storePlaudToken(strings.TrimSpace(pasteToken))
			case useSMS:
				return runPlaudLogin()
			default:
				return runPlaudBrowserLogin(browser, profile)
			}
		},
	}
	c.Flags().StringVar(&pasteToken, "token", "", "store a bearer JWT you already have (grab it from web.plaud.ai devtools)")
	c.Flags().BoolVar(&useSMS, "sms", false, "use the phone + SMS one-time-code flow instead of reading the browser")
	c.Flags().StringVar(&browser, "browser", "chrome", "browser to read the session from (chrome|brave|edge|chromium)")
	c.Flags().StringVar(&profile, "profile", "", "restrict to a browser profile by dir or display name (e.g. \"Profile 1\" or \"Plaud\") — use a dedicated profile so the backend owns the session")
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
	// A bare token from --token / SMS has no refresh material; the backend will
	// use it until it expires (24h for SSO workspace tokens). Decode the wid so
	// the backend at least *could* refresh if a refresh token is added later.
	return storePlaudCreds(map[string]string{
		"plaud_token":        token,
		"plaud_workspace_id": jwtClaimString(token, "wid"),
		"plaud_region":       "", // backend auto-detects region on -302 mismatch
	})
}

// storePlaudCreds merges in linked_at and pushes the full credential set.
func storePlaudCreds(creds map[string]string) error {
	creds["linked_at"] = time.Now().UTC().Format(time.RFC3339)
	if _, ok := creds["plaud_region"]; !ok {
		creds["plaud_region"] = ""
	}
	fmt.Println("Pushing Plaud session to cassandra-auth...")
	if err := pushCredentials(plaudAuthService, creds); err != nil {
		return err
	}
	fmt.Println("Plaud linked. plaud-mcp will mirror your recordings.")
	if rt := creds["plaud_refresh_token"]; rt != "" {
		if exp := jwtExpiry(rt); !exp.IsZero() {
			fmt.Printf("  Auto-refresh works until %s (%dd) — re-run `cass plaud login` after that.\n",
				exp.Format("2006-01-02"), int(time.Until(exp).Hours()/24))
		}
	}
	return nil
}

// ── Browser (Chrome) session capture ───────────────────────────────────────

// plaudWSBlob mirrors the workspace object Plaud's web app persists in
// localStorage (key holds a JSON array of these). Timestamps are epoch ms.
type plaudWSBlob struct {
	WorkspaceID      string `json:"workspaceId"`
	Region           string `json:"region"`
	UserName         string `json:"userName"`
	WorkspaceToken   string `json:"workspaceToken"`
	ExpiresAt        int64  `json:"expiresAt"`
	RefreshToken     string `json:"refreshToken"`
	RefreshExpiresAt int64  `json:"refreshExpiresAt"`
}

// localStorage stores values as a JSON array `[{"workspaceId":...}]`. Match the
// whole array non-greedily; tokens are base64 (no braces) so this is safe.
var plaudWSBlobRE = regexp.MustCompile(`\[\{"workspaceId".*?refreshExpiresAt":\d+\}\]`)

func runPlaudBrowserLogin(browser, profile string) error {
	blob, src, err := findPlaudSession(browser, profile)
	if err != nil {
		return err
	}
	if blob.WorkspaceToken == "" || blob.RefreshToken == "" {
		return fmt.Errorf("found a Plaud session in %s but it has no tokens — open web.plaud.ai, sign in, then retry", browser)
	}
	// src is <profile>/Local Storage/leveldb/<file>; the profile is 3 dirs up.
	profileDir := filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(src))))
	fmt.Printf("Found Plaud session for %s (%s profile).\n", strings.TrimSpace(blob.UserName), profileDir)
	return storePlaudCreds(map[string]string{
		"plaud_token":         blob.WorkspaceToken,
		"plaud_refresh_token": blob.RefreshToken,
		"plaud_workspace_id":  blob.WorkspaceID,
		"plaud_region":        "", // detected server-side; blob.Region kept for reference only
	})
}

// findPlaudSession scans a Chromium-family browser's Local Storage leveldb files
// for the freshest Plaud workspace session. It only READS the files (no leveldb
// open) so it works while the browser is running and holding the DB lock.
func findPlaudSession(browser, profile string) (plaudWSBlob, string, error) {
	roots, err := chromiumLevelDBDirs(browser, profile)
	if err != nil {
		return plaudWSBlob{}, "", err
	}
	var best plaudWSBlob
	var bestSrc string
	scanned := 0
	for _, dir := range roots {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if !strings.HasSuffix(name, ".ldb") && !strings.HasSuffix(name, ".log") {
				continue
			}
			path := filepath.Join(dir, name)
			raw, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			scanned++
			// leveldb may hold values UTF-16-encoded (NUL-interleaved); drop NULs
			// so the regex matches either encoding.
			cleaned := bytes.ReplaceAll(raw, []byte{0}, nil)
			for _, m := range plaudWSBlobRE.FindAll(cleaned, -1) {
				var arr []plaudWSBlob
				if json.Unmarshal(m, &arr) != nil || len(arr) == 0 {
					continue
				}
				b := arr[0]
				if b.WorkspaceToken != "" && b.ExpiresAt > best.ExpiresAt {
					best, bestSrc = b, path
				}
			}
		}
	}
	if bestSrc == "" {
		if scanned == 0 {
			if profile != "" {
				return plaudWSBlob{}, "", fmt.Errorf("no %s profile matching %q — available: %s", browser, profile, strings.Join(chromiumProfileNames(browser), ", "))
			}
			return plaudWSBlob{}, "", fmt.Errorf("no %s profile found — is %s installed? (try --browser brave|edge|chromium, or --token)", browser, browser)
		}
		return plaudWSBlob{}, "", fmt.Errorf("no Plaud session found in %s — sign in at web.plaud.ai in that browser, then retry (or use --token)", browser)
	}
	return best, bestSrc, nil
}

// chromiumBase returns the User-Data root for a Chromium-family browser on this OS.
func chromiumBase(browser string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", chromiumVendorPath(browser)), nil
	case "linux":
		return filepath.Join(home, ".config", chromiumVendorPath(browser)), nil
	case "windows":
		return filepath.Join(os.Getenv("LOCALAPPDATA"), chromiumVendorPath(browser), "User Data"), nil
	default:
		return "", fmt.Errorf("unsupported OS for browser capture: %s", runtime.GOOS)
	}
}

// chromiumLevelDBDirs returns the per-profile Local Storage leveldb dirs for a
// Chromium-family browser. When profile is set, only profiles whose directory
// name OR display name (case-insensitively) matches are returned.
func chromiumLevelDBDirs(browser, profile string) ([]string, error) {
	base, err := chromiumBase(browser)
	if err != nil {
		return nil, err
	}
	var dirs []string
	profiles, _ := os.ReadDir(base)
	for _, p := range profiles {
		if !p.IsDir() {
			continue
		}
		if p.Name() != "Default" && !strings.HasPrefix(p.Name(), "Profile ") {
			continue
		}
		if profile != "" && !profileMatches(base, p.Name(), profile) {
			continue
		}
		dirs = append(dirs, filepath.Join(base, p.Name(), "Local Storage", "leveldb"))
	}
	sort.Strings(dirs)
	return dirs, nil
}

// profileMatches reports whether a profile directory matches the user's filter,
// by directory name or by the display name in its Preferences file.
func profileMatches(base, dirName, filter string) bool {
	f := strings.ToLower(filter)
	if strings.ToLower(dirName) == f {
		return true
	}
	return strings.Contains(strings.ToLower(chromiumProfileDisplayName(base, dirName)), f)
}

func chromiumProfileDisplayName(base, dirName string) string {
	raw, err := os.ReadFile(filepath.Join(base, dirName, "Preferences"))
	if err != nil {
		return ""
	}
	var prefs struct {
		Profile struct {
			Name string `json:"name"`
		} `json:"profile"`
	}
	if json.Unmarshal(raw, &prefs) != nil {
		return ""
	}
	return prefs.Profile.Name
}

// chromiumProfileNames lists "dir (display name)" for each profile, for errors.
func chromiumProfileNames(browser string) []string {
	base, err := chromiumBase(browser)
	if err != nil {
		return nil
	}
	var names []string
	profiles, _ := os.ReadDir(base)
	for _, p := range profiles {
		if !p.IsDir() || (p.Name() != "Default" && !strings.HasPrefix(p.Name(), "Profile ")) {
			continue
		}
		if dn := chromiumProfileDisplayName(base, p.Name()); dn != "" {
			names = append(names, fmt.Sprintf("%q (%s)", p.Name(), dn))
		} else {
			names = append(names, fmt.Sprintf("%q", p.Name()))
		}
	}
	return names
}

func chromiumVendorPath(browser string) string {
	switch strings.ToLower(browser) {
	case "brave":
		return filepath.Join("BraveSoftware", "Brave-Browser")
	case "edge":
		return filepath.Join("Microsoft", "Edge")
	case "chromium":
		return "Chromium"
	default: // chrome
		return filepath.Join("Google", "Chrome")
	}
}

// ── JWT helpers (no signature check — we only read claims we already trust) ──

func jwtPayload(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

func jwtClaimString(token, claim string) string {
	if m := jwtPayload(token); m != nil {
		if s, ok := m[claim].(string); ok {
			return s
		}
	}
	return ""
}

func jwtExpiry(token string) time.Time {
	if m := jwtPayload(token); m != nil {
		if exp, ok := m["exp"].(float64); ok {
			return time.Unix(int64(exp), 0)
		}
	}
	return time.Time{}
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
		fmt.Printf("  linked at:      %s\n", s)
	}
	if s, ok := raw["plaud_region"].(string); ok && s != "" {
		fmt.Printf("  region:         %s\n", s)
	}
	if tok, ok := raw["plaud_token"].(string); ok {
		printTokenExpiry("  access token:   ", tok, "expired — backend will refresh on next sync")
	}
	if rt, ok := raw["plaud_refresh_token"].(string); ok && rt != "" {
		printTokenExpiry("  refresh token:  ", rt, "EXPIRED — run `cass plaud login` to re-capture")
	} else {
		fmt.Println("  refresh token:  none — token won't auto-renew; re-link with `cass plaud login`")
	}
	return nil
}

func printTokenExpiry(label, token, expiredMsg string) {
	exp := jwtExpiry(token)
	if exp.IsZero() {
		return
	}
	if d := time.Until(exp); d > 0 {
		fmt.Printf("%svalid %s (%dd left)\n", label, exp.Format("2006-01-02"), int(d.Hours()/24))
	} else {
		fmt.Printf("%s%s\n", label, expiredMsg)
	}
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
