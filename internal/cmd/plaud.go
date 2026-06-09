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
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Cassandras-Edge/cass/internal/auth"
	"github.com/Cassandras-Edge/cass/internal/browser"
	"github.com/Cassandras-Edge/cass/internal/config"
)

// Plaud auth is NOT OAuth — we read the live web.plaud.ai session out of the
// browser (the SMS one-time-code flow is gone: it mints a phone-only "shadow"
// account separate from the Google/Apple SSO login, so it returns an empty
// account on this tenant). Sources, by durability:
//
//   - BROWSER (default) — lift the session from Chrome's localStorage (leveldb,
//     plaintext) the same way `cass tradingview`/`cass perplexity` lift cookies.
//     Plaud's web session is a TWO-token model:
//     - workspace_token (the bearer used for /file/* calls) — ~24h life
//     - refresh_token                                       — ~27d HARD ceiling
//     We capture both + the workspaceId; the backend refreshes the 24h token
//     from the refresh_token on every sync (POST /user-app/auth/workspace/
//     refresh/{wid}). After ~27d the refresh token expires and you re-run login.
//   - --cookies — capture the long-lived pld_urt user-refresh cookie from
//     Firefox (~10 months); the backend re-bootstraps the whole session from it.
//   - --paste / --token — clipboard or a pasted bearer JWT, for any browser.
//
// The result lands in cassandra-auth under credentials/{email}/plaud-mcp =
// {plaud_token, plaud_refresh_token, plaud_workspace_id, plaud_region, ...} and
// drives cassandra-plaud-mcp's mirror. `plaud_region` is left empty — the
// backend auto-detects on a -302.
const plaudAuthService = "plaud-mcp"

func plaudCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plaud",
		Short: "Link your Plaud account for the plaud-mcp service",
		Long: "Capture your Plaud session and store it in cassandra-auth.\n\n" +
			"By default this reads your live web.plaud.ai session from Chrome's\n" +
			"local storage (workspace token + refresh token). The backend then\n" +
			"refreshes the short-lived token automatically for ~27 days, after\n" +
			"which you re-run this command.\n\n" +
			"Use --cookies for the most durable path (the ~10-month pld_urt cookie\n" +
			"from Firefox), --paste to read the session from your clipboard, or\n" +
			"--token to store a bearer JWT you already have.",
	}
	cmd.AddCommand(plaudLoginCmd())
	cmd.AddCommand(plaudStatusCmd())
	return cmd
}

func plaudLoginCmd() *cobra.Command {
	var pasteToken string
	var paste bool
	var cookies bool
	var browser string
	var profile string
	c := &cobra.Command{
		Use:   "login",
		Short: "Link Plaud — reads your Chrome session by default (--cookies for ~10mo durability)",
		RunE: func(_ *cobra.Command, _ []string) error {
			switch {
			case cookies:
				return runPlaudCookieLogin()
			case paste:
				return runPlaudPasteLogin()
			case pasteToken != "":
				return storePlaudToken(strings.TrimSpace(pasteToken))
			default:
				return runPlaudBrowserLogin(browser, profile)
			}
		},
	}
	c.Flags().StringVar(&pasteToken, "token", "", "store a bearer JWT or a pasted workspaceList JSON blob")
	c.Flags().BoolVar(&paste, "paste", false, "read the session JSON from your clipboard (see the devtools snippet this prints) — works in any browser, even private mode")
	c.Flags().BoolVar(&cookies, "cookies", false, "capture the long-lived pld_urt cookie from Firefox (most durable, ~10 months)")
	c.Flags().StringVar(&browser, "browser", "chrome", "browser to read the session from (chrome|firefox|brave|edge|chromium)")
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

// plaudConsoleSnippet copies the live Plaud session to the clipboard from any
// browser's devtools console (works even when nothing is persisted to disk).
const plaudConsoleSnippet = `copy(localStorage.getItem(Object.keys(localStorage).find(k=>k.endsWith(':workspaceList'))))`

// runPlaudPasteLogin reads the workspaceList JSON blob from the clipboard.
func runPlaudPasteLogin() error {
	fmt.Println("In the browser tab logged into web.plaud.ai, open the devtools")
	fmt.Println("console (Cmd+Opt+K) and paste this, which copies your session:")
	fmt.Println()
	fmt.Println("    " + plaudConsoleSnippet)
	fmt.Println()
	clip, err := readClipboard()
	if err != nil {
		return fmt.Errorf("read clipboard: %w (run the snippet above first)", err)
	}
	clip = strings.TrimSpace(clip)
	if clip == "" {
		return fmt.Errorf("clipboard is empty — run the snippet above, then re-run `cass plaud login --paste`")
	}
	blob, err := parsePlaudSessionBlob(clip)
	if err != nil {
		return fmt.Errorf("clipboard isn't a Plaud session blob: %w", err)
	}
	fmt.Printf("Captured session for %s from clipboard.\n", strings.TrimSpace(blob.UserName))
	return storePlaudCreds(map[string]string{
		"plaud_token":         blob.WorkspaceToken,
		"plaud_refresh_token": blob.RefreshToken,
		"plaud_workspace_id":  blob.WorkspaceID,
		"plaud_region":        "",
	})
}

// parsePlaudSessionBlob accepts the localStorage `<id>:workspaceList` value
// (a JSON array of workspace objects) or a single object, and returns the
// freshest workspace with tokens.
func parsePlaudSessionBlob(s string) (plaudWSBlob, error) {
	s = strings.TrimSpace(s)
	var arr []plaudWSBlob
	if strings.HasPrefix(s, "[") {
		if err := json.Unmarshal([]byte(s), &arr); err != nil {
			return plaudWSBlob{}, err
		}
	} else {
		var one plaudWSBlob
		if err := json.Unmarshal([]byte(s), &one); err != nil {
			return plaudWSBlob{}, err
		}
		arr = []plaudWSBlob{one}
	}
	var best plaudWSBlob
	for _, b := range arr {
		if b.WorkspaceToken != "" && b.ExpiresAt >= best.ExpiresAt {
			best = b
		}
	}
	if best.WorkspaceToken == "" || best.RefreshToken == "" {
		return plaudWSBlob{}, fmt.Errorf("no workspace with both tokens found")
	}
	return best, nil
}

func readClipboard() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("pbpaste").Output()
		return string(out), err
	case "linux":
		if out, err := exec.Command("wl-paste").Output(); err == nil {
			return string(out), nil
		}
		out, err := exec.Command("xclip", "-selection", "clipboard", "-o").Output()
		return string(out), err
	default:
		return "", fmt.Errorf("clipboard unsupported on %s — use --token", runtime.GOOS)
	}
}

func storePlaudToken(token string) error {
	// Accept a pasted workspaceList JSON blob as well as a bare JWT.
	if strings.HasPrefix(token, "[") || strings.HasPrefix(token, "{") {
		blob, err := parsePlaudSessionBlob(token)
		if err != nil {
			return fmt.Errorf("looks like JSON but isn't a Plaud session blob: %w", err)
		}
		return storePlaudCreds(map[string]string{
			"plaud_token":         blob.WorkspaceToken,
			"plaud_refresh_token": blob.RefreshToken,
			"plaud_workspace_id":  blob.WorkspaceID,
			"plaud_region":        "",
		})
	}
	if !strings.HasPrefix(token, "eyJ") {
		fmt.Println("Warning: token doesn't look like a JWT (expected to start with 'eyJ').")
	}
	// A bare token from --token has no refresh material; the backend will
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
	// Report the longest-lived renewal source so you know when to re-link.
	if urt := creds["plaud_urt"]; urt != "" {
		if exp := jwtExpiry(urt); !exp.IsZero() {
			fmt.Printf("  Hands-off until %s (%dd) via the pld_urt cookie — re-run after that.\n",
				exp.Format("2006-01-02"), int(time.Until(exp).Hours()/24))
		}
	} else if rt := creds["plaud_refresh_token"]; rt != "" {
		if exp := jwtExpiry(rt); !exp.IsZero() {
			fmt.Printf("  Auto-refresh works until %s (%dd) — re-run `cass plaud login` after that.\n",
				exp.Format("2006-01-02"), int(time.Until(exp).Hours()/24))
		}
	}
	return nil
}

// fetchPlaudCreds reads the currently-stored plaud-mcp credentials (best-effort)
// so a partial capture (e.g. just the pld_urt cookie) can merge instead of
// clobbering the existing session.
func fetchPlaudCreds() map[string]string {
	out := map[string]string{}
	creds, err := auth.Read()
	if err != nil {
		return out
	}
	u := fmt.Sprintf("%s/api/extension/credentials/%s", config.PortalURL(), plaudAuthService)
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("CF-Access-Client-Id", creds.CFAccessClientID)
	req.Header.Set("CF-Access-Client-Secret", creds.CFAccessClientSecret)
	req.Header.Set("Authorization", "Bearer "+creds.MCPKey)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return out
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return out
	}
	var raw map[string]any
	if json.NewDecoder(resp.Body).Decode(&raw) != nil {
		return out
	}
	if inner, ok := raw["credentials"].(map[string]any); ok {
		raw = inner
	}
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// runPlaudCookieLogin captures the long-lived pld_urt user-refresh cookie from
// Firefox (plaintext cookies.sqlite, same as the other cass cookie lifts). The
// backend re-bootstraps the whole workspace session from it, so this is the
// durable path — good for ~10 months instead of the workspace token's ~30 days.
func runPlaudCookieLogin() error {
	urt, exp := browser.ReadFirefoxCookie("%plaud.ai%", "pld_urt")
	if urt == "" {
		return fmt.Errorf("no pld_urt cookie in Firefox — sign into web.plaud.ai in Firefox with normal " +
			"(non-private) settings, reload the page once, then retry (or use --paste)")
	}
	merged := fetchPlaudCreds()
	merged["plaud_urt"] = urt
	// The backend needs the workspace id to bootstrap; reuse the stored one, or
	// discover it from a Chromium localStorage session if we've never linked.
	if merged["plaud_workspace_id"] == "" {
		if blob, _, e := findPlaudSession("chrome", ""); e == nil && blob.WorkspaceID != "" {
			merged["plaud_workspace_id"] = blob.WorkspaceID
		}
	}
	if merged["plaud_workspace_id"] == "" {
		return fmt.Errorf("workspace id unknown — run `cass plaud login` (Chrome) once to capture it, then `cass plaud login --cookies`")
	}
	days := int(time.Until(time.Unix(exp, 0)).Hours() / 24)
	fmt.Printf("Captured pld_urt cookie from Firefox (valid ~%dd). Backend now owns the session.\n", days)
	fmt.Println("Tip: don't actively browse web.plaud.ai in this Firefox — it rotates the cookie out from under the backend.")
	return storePlaudCreds(merged)
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
	fmt.Printf("Found Plaud session for %s (%s, %s profile).\n",
		strings.TrimSpace(blob.UserName), browser, sessionProfileName(browser, src))
	return storePlaudCreds(map[string]string{
		"plaud_token":         blob.WorkspaceToken,
		"plaud_refresh_token": blob.RefreshToken,
		"plaud_workspace_id":  blob.WorkspaceID,
		"plaud_region":        "", // detected server-side; blob.Region kept for reference only
	})
}

// findPlaudSession scans a browser's localStorage backing files for the freshest
// Plaud workspace session. It only READS the files (no DB open) so it works
// while the browser is running and holding the file lock — Chromium keeps
// localStorage in a leveldb (.ldb/.log), Firefox in webappsstore.sqlite(-wal).
func findPlaudSession(browser, profile string) (plaudWSBlob, string, error) {
	files, err := plaudSessionFiles(browser, profile)
	if err != nil {
		return plaudWSBlob{}, "", err
	}
	var best plaudWSBlob
	var bestSrc string
	scanned := 0
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		scanned++
		// Values may be UTF-16-encoded (NUL-interleaved); drop NULs so the
		// regex matches either encoding.
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
	if bestSrc == "" {
		if scanned == 0 {
			if profile != "" && !isFirefox(browser) {
				return plaudWSBlob{}, "", fmt.Errorf("no %s profile matching %q — available: %s", browser, profile, strings.Join(chromiumProfileNames(browser), ", "))
			}
			return plaudWSBlob{}, "", fmt.Errorf("no %s profile/localStorage found — is %s installed? (try --browser firefox|brave|edge, or --token)", browser, browser)
		}
		return plaudWSBlob{}, "", fmt.Errorf("no Plaud session found in %s — sign in at web.plaud.ai in that browser, then retry (or use --token)", browser)
	}
	return best, bestSrc, nil
}

func isFirefox(browser string) bool { return strings.ToLower(browser) == "firefox" }

// plaudSessionFiles returns the localStorage backing files to scan for a
// browser, restricted to a profile when given.
func plaudSessionFiles(browser, profile string) ([]string, error) {
	if isFirefox(browser) {
		return firefoxLocalStorageFiles(profile)
	}
	dirs, err := chromiumLevelDBDirs(browser, profile)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, dir := range dirs {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if n := e.Name(); strings.HasSuffix(n, ".ldb") || strings.HasSuffix(n, ".log") {
				files = append(files, filepath.Join(dir, n))
			}
		}
	}
	return files, nil
}

// firefoxLocalStorageFiles returns webappsstore.sqlite(+ -wal) for each Firefox
// profile (filtered by directory-name substring when profile is set).
func firefoxLocalStorageFiles(profile string) ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	var base string
	switch runtime.GOOS {
	case "darwin":
		base = filepath.Join(home, "Library", "Application Support", "Firefox", "Profiles")
	case "linux":
		base = filepath.Join(home, ".mozilla", "firefox")
	case "windows":
		base = filepath.Join(os.Getenv("APPDATA"), "Mozilla", "Firefox", "Profiles")
	default:
		return nil, fmt.Errorf("unsupported OS for browser capture: %s", runtime.GOOS)
	}
	profiles, _ := os.ReadDir(base)
	var files []string
	for _, p := range profiles {
		if !p.IsDir() {
			continue
		}
		if profile != "" && !strings.Contains(strings.ToLower(p.Name()), strings.ToLower(profile)) {
			continue
		}
		for _, f := range []string{"webappsstore.sqlite", "webappsstore.sqlite-wal"} {
			files = append(files, filepath.Join(base, p.Name(), f))
		}
	}
	return files, nil
}

// sessionProfileName extracts the profile directory name from a scanned file
// path for display (depth differs between Chromium and Firefox layouts).
func sessionProfileName(browser, src string) string {
	if isFirefox(browser) {
		// .../Profiles/<profile>/webappsstore.sqlite
		return filepath.Base(filepath.Dir(src))
	}
	// .../<profile>/Local Storage/leveldb/<file>
	return filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(src))))
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
