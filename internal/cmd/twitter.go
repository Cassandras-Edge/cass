package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"

	"github.com/Cassandras-Edge/cass/internal/auth"
	"github.com/Cassandras-Edge/cass/internal/config"
)

const twitterUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:135.0) Gecko/20100101 Firefox/135.0"

var (
	bundleURLPattern = regexp.MustCompile(`(?:src|href)=["'](https://abs\.twimg\.com/responsive-web/client-web[^"']+\.js)["']`)
	opPattern        = regexp.MustCompile(`queryId:\s*"([A-Za-z0-9_-]+)"[^}]{0,200}operationName:\s*"([^"]+)"`)
)

func twitterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "twitter",
		Short: "X / Twitter helper commands",
	}
	cmd.AddCommand(twitterLoginCmd())
	cmd.AddCommand(twitterStatusCmd())
	cmd.AddCommand(twitterSyncQueryIDsCmd())
	return cmd
}

func twitterSyncQueryIDsCmd() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "sync-queryids",
		Short: "Scrape current X GraphQL queryIds from Firefox and push to auth",
		RunE: func(_ *cobra.Command, _ []string) error {
			fmt.Println("Pulling x.com cookies from firefox...")
			cookies, err := firefoxCookiesFor("https://x.com", []string{".x.com", ".twitter.com"})
			if err != nil {
				return err
			}
			if len(cookies) == 0 {
				return fmt.Errorf("no x.com cookies in firefox. Run: cass cookies sync twitter")
			}
			fmt.Println("Scraping x.com JS bundles for queryIds...")
			ids, err := scrapeXQueryIDs(cookies)
			if err != nil {
				return err
			}
			if len(ids) == 0 {
				return fmt.Errorf("no queryIds extracted — bundle layout may have changed")
			}
			fmt.Printf("  Found %d operations.\n", len(ids))
			ops := make([]string, 0, len(ids))
			for op := range ids {
				ops = append(ops, op)
			}
			sort.Strings(ops)
			for _, op := range ops {
				fmt.Printf("    %-32s %s\n", op, ids[op])
			}
			if dryRun {
				fmt.Println("Dry run — not pushing.")
				return nil
			}
			applied, err := pushTwitterQueryIDs(ids)
			if err != nil {
				return err
			}
			fmt.Printf("Pushed %d ids to %s/admin/queryids — %d applied live ✓\n",
				len(ids), config.TwitterMcpURL(), applied)
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Scrape and print, don't push")
	return cmd
}

// firefoxCookiesFor shells out to yt-dlp to extract cookies for a probe URL
// from Firefox, then returns a name→value map filtered by the given domains.
// yt-dlp handles the Firefox SQLite + decryption details for us.
func firefoxCookiesFor(probeURL string, domains []string) (map[string]string, error) {
	if _, err := exec.LookPath("yt-dlp"); err != nil {
		return nil, fmt.Errorf("yt-dlp required. Install: brew install yt-dlp")
	}
	tmp, err := os.MkdirTemp("", "cass-cookies-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	out := filepath.Join(tmp, "cookies.txt")
	args := []string{
		"--cookies-from-browser", "firefox",
		"--cookies", out,
		"--flat-playlist", "--skip-download", "--no-warnings",
		probeURL,
	}
	_ = exec.Command("yt-dlp", args...).Run() // ignore exit; we check the file
	data, err := os.ReadFile(out)
	if err != nil || len(data) == 0 {
		return nil, nil
	}
	return parseNetscapeJar(string(data), domains), nil
}

func parseNetscapeJar(jar string, domains []string) map[string]string {
	jarOut := map[string]string{}
	for _, line := range strings.Split(jar, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 7 {
			continue
		}
		host, name, value := parts[0], parts[5], parts[6]
		for _, d := range domains {
			if host == d || strings.HasSuffix(host, d) {
				jarOut[name] = value
				break
			}
		}
	}
	return jarOut
}

func scrapeXQueryIDs(cookies map[string]string) (map[string]string, error) {
	jar, _ := cookiejar.New(nil)
	u, _ := url.Parse("https://x.com/")
	httpCookies := make([]*http.Cookie, 0, len(cookies))
	for k, v := range cookies {
		httpCookies = append(httpCookies, &http.Cookie{Name: k, Value: v})
	}
	jar.SetCookies(u, httpCookies)
	client := &http.Client{
		Timeout: 30 * time.Second,
		Jar:     jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	req, _ := http.NewRequest("GET", "https://x.com/", nil)
	req.Header.Set("User-Agent", twitterUA)
	req.Header.Set("Accept-Language", "en-US,en;q=0.5")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	matches := bundleURLPattern.FindAllStringSubmatch(string(body), -1)
	if len(matches) == 0 {
		return nil, fmt.Errorf("no JS bundles found on x.com — cookies may be stale. Run: cass cookies sync twitter")
	}
	seen := map[string]bool{}
	var urls []string
	for _, m := range matches {
		if !seen[m[1]] {
			seen[m[1]] = true
			urls = append(urls, m[1])
		}
	}
	result := map[string]string{}
	for _, u := range urls {
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("User-Agent", twitterUA)
		r, err := client.Do(req)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  bundle fetch failed (%s): %v\n", filepath.Base(u), err)
			continue
		}
		if r.StatusCode != 200 {
			r.Body.Close()
			continue
		}
		buf, _ := io.ReadAll(r.Body)
		r.Body.Close()
		for _, m := range opPattern.FindAllStringSubmatch(string(buf), -1) {
			queryID, opName := m[1], m[2]
			if _, ok := result[opName]; !ok {
				result[opName] = queryID
			}
		}
	}
	return result, nil
}

// pushTwitterQueryIDs POSTs the scraped queryIds to twitter-mcp's /admin/queryids
// endpoint. The service persists to auth and patches its in-process cache, so a
// successful response means the live pod is already serving the new IDs — no
// pod restart, no port-forward into auth.
//
// Returns (applied, err) where `applied` is the number of cache entries that
// actually changed in-process. Auth is the shared admin secret from env/acl.env.
func pushTwitterQueryIDs(ids map[string]string) (int, error) {
	secret := config.AuthSecret()
	if secret == "" {
		return 0, fmt.Errorf("AUTH_SECRET not set — /admin/queryids needs the shared admin secret (env/acl.env)")
	}
	body, _ := json.Marshal(map[string]any{"queryIds": ids})
	req, _ := http.NewRequest("POST", config.TwitterMcpURL()+"/admin/queryids", bytes.NewReader(body))
	req.Header.Set("X-Auth-Secret", secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return 0, fmt.Errorf("twitter-mcp %s: %s", resp.Status, string(buf))
	}
	var out struct {
		Applied int `json:"applied"`
	}
	buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_ = json.Unmarshal(buf, &out)
	return out.Applied, nil
}

// ─── X login (mint auth_token + ct0 without scraping the browser) ───────────
//
// cass drives X's onboarding/login flow directly: guest token → a flow_token
// chain of subtasks (username, password, optional 2FA / email challenge) → the
// auth_token + ct0 session cookies twitter-mcp needs. Same destination as
// `cass cookies sync twitter` (auth service, keys twitter_auth_token /
// twitter_ct0) — just obtained by logging in instead of reading Firefox.
//
// Running it on YOUR machine (residential IP, same as your browser) is far less
// likely to trip X's anti-automation than a server-side login. Even so, X may
// inject an Arkose/FunCaptcha challenge the headless flow can't solve — for that
// case there's the --auth-token/--ct0 paste escape hatch (grab both from x.com
// devtools → Application → Cookies).

const (
	// xWebBearer is the public web-app bearer x.com's own JS ships with — used
	// for guest activation + the onboarding flow. Not a secret.
	xWebBearer = "AAAAAAAAAAAAAAAAAAAAANRILgAAAAAAnNwIzUejRCOuH5E6I8xnZz4puTs%3D1Zv7ttfk8LF81IUq16cHjhLTvJu4FA33AGWWjCpTnA"
	xAPIBase   = "https://api.x.com"
	// twitterAuthService matches where `cass cookies sync twitter` stores the
	// session, so both paths feed twitter-mcp's per-user ACL credentials.
	twitterAuthService = "twitter"
)

// xSubtaskVersions mirrors the version map the real web client sends on flow
// init. X rejects unknown/missing versions for some subtasks, so we ship the
// full set the login flow may traverse.
var xSubtaskVersions = map[string]any{
	"action_list": 2, "alert_dialog": 1, "app_download_cta": 1,
	"check_logged_in_account": 1, "choice_selection": 3,
	"contacts_live_sync_permission_prompt": 0, "cta": 7, "email_verification": 2,
	"end_flow": 1, "enter_date": 1, "enter_email": 2, "enter_password": 5,
	"enter_phone": 2, "enter_recaptcha": 1, "enter_text": 5, "enter_username": 2,
	"generic_urt": 3, "in_app_notification": 1, "interest_picker": 3,
	"js_instrumentation": 1, "menu_dialog": 1, "notifications_permission_prompt": 2,
	"open_account": 2, "open_home_timeline": 1, "open_link": 1,
	"phone_verification": 4, "privacy_options": 1, "security_key": 3,
	"select_avatar": 4, "select_banner": 2, "settings_list": 7, "show_code": 1,
	"sign_up": 2, "sign_up_review": 4, "tweet_selection_urt": 1, "update_users": 1,
	"upload_media": 1, "user_recommendations_list": 4, "user_recommendations_urt": 1,
	"wait_spinner": 3, "web_modal": 1,
}

func twitterLoginCmd() *cobra.Command {
	var authToken, ct0 string
	c := &cobra.Command{
		Use:   "login",
		Short: "Log in to X and store auth_token + ct0 in cassandra-auth (no browser scrape)",
		Long: "Drive X's login flow directly from this machine and push the resulting\n" +
			"auth_token + ct0 session cookies to cassandra-auth — the same credentials\n" +
			"`cass cookies sync twitter` stores, minted by logging in instead of reading\n" +
			"Firefox. Handles password + 2FA / email challenges interactively.\n\n" +
			"If X blocks the headless flow with a captcha, grab auth_token and ct0 from\n" +
			"x.com devtools and pass them with --auth-token / --ct0.",
		RunE: func(_ *cobra.Command, _ []string) error {
			if authToken != "" || ct0 != "" {
				if authToken == "" || ct0 == "" {
					return errors.New("--auth-token and --ct0 must be provided together")
				}
				return storeTwitterCookies(strings.TrimSpace(authToken), strings.TrimSpace(ct0))
			}
			return runTwitterLogin()
		},
	}
	c.Flags().StringVar(&authToken, "auth-token", "", "skip login and store an auth_token you already have (from x.com devtools)")
	c.Flags().StringVar(&ct0, "ct0", "", "the matching ct0 cookie (paired with --auth-token)")
	return c
}

func twitterStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether your X account is linked",
		RunE: func(_ *cobra.Command, _ []string) error {
			return runTwitterStatus()
		},
	}
}

func runTwitterLogin() error {
	var username, password string
	form := huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title("X username, email, or phone").
			Value(&username).
			Validate(requiredField),
		huh.NewInput().
			Title("Password").
			EchoMode(huh.EchoModePassword).
			Value(&password).
			Validate(requiredField),
	))
	if err := form.Run(); err != nil {
		return err
	}
	username = strings.TrimSpace(username)
	password = strings.TrimSpace(password)

	client, err := xOnboardingClient()
	if err != nil {
		return err
	}

	fmt.Println("Requesting guest token...")
	guest, err := xGuestToken(client)
	if err != nil {
		return fmt.Errorf("guest token: %w", err)
	}

	fmt.Println("Starting X login flow...")
	flowToken, subtasks, err := xFlowInit(client, guest)
	if err != nil {
		return fmt.Errorf("flow init: %w", err)
	}

	// Walk the subtask chain. Bounded so a misbehaving flow can't loop forever.
	// prevID lets us tell when X re-issues the same step — i.e. it rejected the
	// last input (a wrong 2FA / confirmation code), so we re-prompt with a note.
	prevID := ""
	for step := 0; step < 20; step++ {
		id := firstSubtaskID(subtasks)
		switch id {
		case "LoginSuccessSubtask":
			return finishTwitterLogin(client)
		case "DenyLoginSubtask":
			return errors.New("X denied the login (suspicious activity or rate limit). " +
				"Wait and retry, or use --auth-token/--ct0 from your browser.")
		case "":
			return errors.New("X returned no actionable subtask before success — the flow may have changed; " +
				"use the --auth-token/--ct0 fallback")
		}
		input, err := buildSubtaskInput(id, username, password, id == prevID)
		if err != nil {
			return err
		}
		flowToken, subtasks, err = xFlowStep(client, guest, flowToken, input)
		if err != nil {
			return fmt.Errorf("subtask %s: %w", id, err)
		}
		prevID = id
	}
	return errors.New("login did not converge after 20 steps — use the --auth-token/--ct0 fallback")
}

// buildSubtaskInput maps an X subtask_id to the JSON input the flow expects,
// prompting interactively for codes X challenges us with (2FA / email / phone).
// retry is true when X re-issued the same subtask (the previous input was
// rejected), so the interactive prompts say so instead of looking identical.
func buildSubtaskInput(id, username, password string, retry bool) (map[string]any, error) {
	switch id {
	case "LoginJsInstrumentationSubtask":
		// The JS challenge is lenient on the login flow; an empty response passes.
		return map[string]any{"subtask_id": id,
			"js_instrumentation": map[string]any{"response": "{}", "link": "next_link"}}, nil
	case "LoginEnterUserIdentifierSSO":
		return map[string]any{"subtask_id": id, "settings_list": map[string]any{
			"setting_responses": []any{map[string]any{
				"key":           "user_identifier",
				"response_data": map[string]any{"text_data": map[string]any{"result": username}},
			}},
			"link": "next_link",
		}}, nil
	case "LoginEnterUserIdentifier":
		return map[string]any{"subtask_id": id,
			"enter_text": map[string]any{"text": username, "link": "next_link"}}, nil
	case "LoginEnterPassword":
		return map[string]any{"subtask_id": id,
			"enter_password": map[string]any{"password": password, "link": "next_link"}}, nil
	case "AccountDuplicationCheck":
		return map[string]any{"subtask_id": id,
			"check_logged_in_account": map[string]any{"link": "AccountDuplicationCheck_false"}}, nil
	case "LoginTwoFactorAuthChallenge":
		title := "Two-factor code (from your authenticator app)"
		if retry {
			title = "That code was rejected — re-enter your two-factor code"
		}
		code, err := promptLine(title)
		if err != nil {
			return nil, err
		}
		return map[string]any{"subtask_id": id,
			"enter_text": map[string]any{"text": code, "link": "next_link"}}, nil
	case "LoginAcid":
		title := "X sent a confirmation — enter the code (or the email/phone it asked for)"
		if retry {
			title = "That was rejected — re-enter the confirmation X asked for"
		}
		val, err := promptLine(title)
		if err != nil {
			return nil, err
		}
		return map[string]any{"subtask_id": id,
			"enter_text": map[string]any{"text": val, "link": "next_link"}}, nil
	default:
		return nil, fmt.Errorf("X requested an unsupported step %q (likely a captcha/Arkose challenge). "+
			"Log in at x.com in a browser, then run: cass twitter login --auth-token <...> --ct0 <...>", id)
	}
}

func finishTwitterLogin(client *http.Client) error {
	authToken, ct0 := xSessionCookies(client)
	if ct0 == "" {
		// The flow occasionally sets ct0 only on the first authenticated hit.
		_ = xWarmCookies(client)
		at2, ct02 := xSessionCookies(client)
		if authToken == "" {
			authToken = at2
		}
		ct0 = ct02
	}
	if authToken == "" || ct0 == "" {
		return fmt.Errorf("login reported success but the session cookies were missing "+
			"(auth_token present=%v, ct0 present=%v)", authToken != "", ct0 != "")
	}
	if handle, err := xVerify(authToken, ct0); err != nil {
		fmt.Printf("Session minted (could not verify handle: %v) — storing anyway.\n", err)
	} else if handle != "" {
		fmt.Printf("Logged in as @%s\n", handle)
	}
	return storeTwitterCookies(authToken, ct0)
}

func storeTwitterCookies(authToken, ct0 string) error {
	fmt.Println("Pushing X session to cassandra-auth...")
	if err := pushCredentials(twitterAuthService, map[string]string{
		"twitter_auth_token": authToken,
		"twitter_ct0":        ct0,
		"linked_at":          time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		return err
	}
	fmt.Println("X linked. twitter-mcp will use this session for your personal-account tools.")
	return nil
}

func runTwitterStatus() error {
	creds, err := auth.Read()
	if err != nil {
		return fmt.Errorf("not logged in (run: cass login): %w", err)
	}
	u := fmt.Sprintf("%s/api/extension/credentials/%s", config.PortalURL(), twitterAuthService)
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("CF-Access-Client-Id", creds.CFAccessClientID)
	req.Header.Set("CF-Access-Client-Secret", creds.CFAccessClientSecret)
	req.Header.Set("Authorization", "Bearer "+creds.MCPKey)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	notLinked := func() {
		fmt.Println("X: NOT LINKED")
		fmt.Println("  Run: cass twitter login")
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNoContent {
		notLinked()
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("portal %s: %s", resp.Status, string(body))
	}
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return err
	}
	if inner, ok := raw["credentials"].(map[string]any); ok {
		raw = inner
	}
	if raw["twitter_auth_token"] == nil {
		notLinked()
		return nil
	}
	fmt.Println("X: LINKED")
	if s, ok := raw["linked_at"].(string); ok && s != "" {
		fmt.Printf("  linked at: %s\n", s)
	}
	return nil
}

// ── X onboarding HTTP plumbing ──────────────────────────────────────────────

func xOnboardingClient() (*http.Client, error) {
	jar, err := cookiejar.New(nil) // carries the att + guest cookies across steps
	if err != nil {
		return nil, err
	}
	return &http.Client{Timeout: 30 * time.Second, Jar: jar}, nil
}

func xGuestToken(client *http.Client) (string, error) {
	req, _ := http.NewRequest("POST", xAPIBase+"/1.1/guest/activate.json", nil)
	req.Header.Set("Authorization", "Bearer "+xWebBearer)
	req.Header.Set("User-Agent", twitterUA)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", resp.Status, xErrMsg(raw))
	}
	var out struct {
		GuestToken string `json:"guest_token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.GuestToken == "" {
		return "", errors.New("no guest_token in response")
	}
	return out.GuestToken, nil
}

func xFlowInit(client *http.Client, guest string) (string, []map[string]any, error) {
	body := map[string]any{
		"input_flow_data": map[string]any{
			"flow_context": map[string]any{
				"debug_overrides": map[string]any{},
				"start_location":  map[string]any{"location": "splash_screen"},
			},
		},
		"subtask_versions": xSubtaskVersions,
	}
	return xPostOnboarding(client, guest, "flow_name=login", body)
}

func xFlowStep(client *http.Client, guest, flowToken string, input map[string]any) (string, []map[string]any, error) {
	body := map[string]any{
		"flow_token":     flowToken,
		"subtask_inputs": []any{input},
	}
	return xPostOnboarding(client, guest, "", body)
}

// xPostOnboarding POSTs to onboarding/task.json and returns (flow_token, subtasks).
func xPostOnboarding(client *http.Client, guest, query string, body map[string]any) (string, []map[string]any, error) {
	buf, _ := json.Marshal(body)
	u := xAPIBase + "/1.1/onboarding/task.json"
	if query != "" {
		u += "?" + query
	}
	req, _ := http.NewRequest("POST", u, bytes.NewReader(buf))
	req.Header.Set("Authorization", "Bearer "+xWebBearer)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", twitterUA)
	req.Header.Set("x-guest-token", guest)
	req.Header.Set("x-twitter-active-user", "yes")
	req.Header.Set("x-twitter-client-language", "en")
	// X sets a ct0 cookie early in the flow and the web client echoes it as
	// x-csrf-token on every onboarding/task.json call (re-read from the cookie
	// jar before each request). Set it unconditionally — empty string until ct0
	// exists, which matches trevorhobenshield's update_token and is a no-op on
	// the guest portion, then satisfies CSRF enforcement on password/2FA steps.
	req.Header.Set("x-csrf-token", xCookie(client, "ct0"))
	// X sets an `att` cookie on the first onboarding response; the web client
	// echoes it as an `att` request header on subsequent steps. Forward it when
	// present so mid-flow steps aren't rejected.
	if att := xCookie(client, "att"); att != "" {
		req.Header.Set("att", att)
	}
	// Once auth_token lands (after the password/dup-check step) the web client
	// flips auth-type on any further onboarding POST. Harmless before then.
	if xCookie(client, "auth_token") != "" {
		req.Header.Set("x-twitter-auth-type", "OAuth2Client")
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", nil, fmt.Errorf("%s: %s", resp.Status, xErrMsg(raw))
	}
	var out struct {
		FlowToken string           `json:"flow_token"`
		Subtasks  []map[string]any `json:"subtasks"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", nil, fmt.Errorf("decode flow response: %w", err)
	}
	return out.FlowToken, out.Subtasks, nil
}

func firstSubtaskID(subtasks []map[string]any) string {
	for _, s := range subtasks {
		if id, _ := s["subtask_id"].(string); id != "" {
			return id
		}
	}
	return ""
}

// xOnboardingHosts are the hosts the login flow may set its cookies on.
var xOnboardingHosts = []string{"https://x.com/", "https://api.x.com/", "https://twitter.com/"}

// xCookie reads a single named cookie from the client's jar across the
// onboarding hosts, returning the last non-empty value found (or "" if absent).
func xCookie(client *http.Client, name string) string {
	var val string
	for _, raw := range xOnboardingHosts {
		u, _ := url.Parse(raw)
		for _, c := range client.Jar.Cookies(u) {
			if c.Name == name && c.Value != "" {
				val = c.Value
			}
		}
	}
	return val
}

// xSessionCookies reads auth_token + ct0 from the client's jar across the x.com
// / api.x.com / twitter.com hosts the flow may have set them on.
func xSessionCookies(client *http.Client) (authToken, ct0 string) {
	for _, raw := range xOnboardingHosts {
		u, _ := url.Parse(raw)
		for _, c := range client.Jar.Cookies(u) {
			switch c.Name {
			case "auth_token":
				if c.Value != "" {
					authToken = c.Value
				}
			case "ct0":
				if c.Value != "" {
					ct0 = c.Value
				}
			}
		}
	}
	return authToken, ct0
}

// xWarmCookies hits x.com once so it sets ct0 if the login flow didn't.
func xWarmCookies(client *http.Client) error {
	req, _ := http.NewRequest("GET", "https://x.com/", nil)
	req.Header.Set("User-Agent", twitterUA)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	return nil
}

// xVerify confirms the minted session works and returns the screen_name. It
// uses a fresh jar-less client so the explicit Cookie header is sent exactly
// once (the onboarding client's jar would otherwise duplicate the cookies).
func xVerify(authToken, ct0 string) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, _ := http.NewRequest("GET", xAPIBase+"/1.1/account/settings.json", nil)
	req.Header.Set("Authorization", "Bearer "+xWebBearer)
	req.Header.Set("User-Agent", twitterUA)
	req.Header.Set("x-twitter-active-user", "yes")
	req.Header.Set("x-csrf-token", ct0)
	req.Header.Set("Cookie", fmt.Sprintf("auth_token=%s; ct0=%s", authToken, ct0))
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("verify: %s", resp.Status)
	}
	var out struct {
		ScreenName string `json:"screen_name"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.ScreenName, nil
}

func xErrMsg(raw []byte) string {
	var e struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(raw, &e) == nil && len(e.Errors) > 0 && e.Errors[0].Message != "" {
		return e.Errors[0].Message
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

func promptLine(title string) (string, error) {
	var v string
	err := huh.NewForm(huh.NewGroup(
		huh.NewInput().Title(title).Value(&v).Validate(requiredField),
	)).Run()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(v), nil
}

func requiredField(s string) error {
	if strings.TrimSpace(s) == "" {
		return errors.New("required")
	}
	return nil
}
