package cmd

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/mdp/qrterminal/v3"
	"github.com/spf13/cobra"

	"github.com/Cassandras-Edge/cass/internal/auth"
	"github.com/Cassandras-Edge/cass/internal/config"
)

const (
	discordRemoteAuthURL = "wss://remote-auth-gateway.discord.gg/?v=2"
	discordOrigin        = "https://discord.com"
	discordUserAgent     = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
)

func discordCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "discord",
		Short: "Manage Discord authentication",
	}
	cmd.AddCommand(discordLoginCmd())
	return cmd
}

func discordLoginCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Authenticate with Discord via QR code scan",
		Long: `Displays a QR code in your terminal. Scan it with the Discord mobile
app to link your account. The token is pushed to the auth service.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Println("Starting Discord QR login...")
			token, err := runDiscordQRLogin(cmd.Context())
			if err != nil {
				return err
			}
			if token == "" {
				return fmt.Errorf("Discord login failed")
			}
			fmt.Println("\nPushing token to auth service...")
			if err := pushDiscordToken(token); err != nil {
				return fmt.Errorf("store token: %w", err)
			}
			fmt.Println("Done — Discord token stored. Bridge will provision automatically.")
			return nil
		},
	}
}

// runDiscordQRLogin executes the remote-auth handshake against Discord. The
// flow:
//
//  1. Generate RSA-2048 keypair locally; send the public key (SPKI DER, base64)
//     to Discord.
//  2. Discord sends a `nonce_proof` payload encrypted under our public key.
//     Decrypt with RSA-OAEP-SHA256, hash with SHA256, send back urlsafe-b64
//     (no padding).
//  3. Discord sends a fingerprint → render QR for https://discord.com/ra/<fp>.
//  4. User scans on mobile → Discord sends user_payload (encrypted) → we
//     decrypt and show the username.
//  5. User approves → Discord sends a ticket → we exchange it for an
//     encrypted_token via REST, decrypt, return.
func runDiscordQRLogin(ctx context.Context) (string, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", err
	}
	spki, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return "", err
	}
	encodedPublicKey := base64.StdEncoding.EncodeToString(spki)

	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 15 * time.Second
	header := http.Header{}
	header.Set("Origin", discordOrigin)
	header.Set("User-Agent", discordUserAgent)
	ws, _, err := dialer.DialContext(ctx, discordRemoteAuthURL, header)
	if err != nil {
		return "", fmt.Errorf("connect to Discord: %w", err)
	}
	defer ws.Close()

	heartbeatStop := make(chan struct{})
	defer close(heartbeatStop)

	decrypt := func(encrypted string) ([]byte, error) {
		raw, err := base64.StdEncoding.DecodeString(encrypted)
		if err != nil {
			return nil, err
		}
		return rsa.DecryptOAEP(sha256.New(), rand.Reader, priv, raw, nil)
	}

	for {
		_, raw, err := ws.ReadMessage()
		if err != nil {
			if isWSExpired(err) {
				return "", fmt.Errorf("QR code expired — run again")
			}
			return "", fmt.Errorf("read from Discord: %w", err)
		}
		var msg map[string]any
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		op, _ := msg["op"].(string)
		switch op {
		case "hello":
			hbMS, _ := msg["heartbeat_interval"].(float64)
			if hbMS <= 0 {
				hbMS = 41250
			}
			go runDiscordHeartbeat(ws, time.Duration(hbMS)*time.Millisecond, heartbeatStop)
			if err := ws.WriteJSON(map[string]string{
				"op":                 "init",
				"encoded_public_key": encodedPublicKey,
			}); err != nil {
				return "", err
			}

		case "nonce_proof":
			encrypted, _ := msg["encrypted_nonce"].(string)
			plain, err := decrypt(encrypted)
			if err != nil {
				return "", fmt.Errorf("decrypt nonce: %w", err)
			}
			h := sha256.Sum256(plain)
			proof := base64.RawURLEncoding.EncodeToString(h[:])
			if err := ws.WriteJSON(map[string]string{
				"op":    "nonce_proof",
				"proof": proof,
			}); err != nil {
				return "", err
			}

		case "pending_remote_init":
			fingerprint, _ := msg["fingerprint"].(string)
			renderDiscordQR("https://discord.com/ra/" + fingerprint)

		case "pending_ticket":
			encrypted, _ := msg["encrypted_user_payload"].(string)
			plain, err := decrypt(encrypted)
			if err != nil {
				return "", fmt.Errorf("decrypt user payload: %w", err)
			}
			parts := strings.Split(string(plain), ":")
			username := "unknown"
			if len(parts) > 3 {
				username = parts[3]
			}
			fmt.Printf("\nUser scanned: %s\n", username)
			fmt.Println("Waiting for approval on mobile...")

		case "pending_login":
			ticket, _ := msg["ticket"].(string)
			fingerprint, _ := fetchDiscordFingerprint(ctx)
			encryptedToken, err := exchangeDiscordTicket(ctx, ticket, fingerprint)
			if err != nil {
				return "", err
			}
			plain, err := decrypt(encryptedToken)
			if err != nil {
				return "", fmt.Errorf("decrypt token: %w", err)
			}
			return string(plain), nil

		case "cancel":
			return "", fmt.Errorf("Discord login was cancelled")
		}
	}
}

func runDiscordHeartbeat(ws *websocket.Conn, interval time.Duration, stop <-chan struct{}) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			if err := ws.WriteJSON(map[string]string{"op": "heartbeat"}); err != nil {
				return
			}
		}
	}
}

func isWSExpired(err error) bool {
	if e, ok := err.(*websocket.CloseError); ok {
		return e.Code == 4003
	}
	return strings.Contains(err.Error(), "4003")
}

func renderDiscordQR(url string) {
	fmt.Println("\nScan this QR code with the Discord mobile app:")
	fmt.Println()
	qrterminal.GenerateWithConfig(url, qrterminal.Config{
		Level:     qrterminal.L,
		Writer:    os.Stdout,
		BlackChar: qrterminal.BLACK,
		WhiteChar: qrterminal.WHITE,
		QuietZone: 1,
	})
	fmt.Println()
	fmt.Printf("Or open this URL on your phone: %s\n", url)
}

// exchangeDiscordTicket trades the ticket for an encrypted token. Discord
// sometimes gates this with an hCaptcha challenge — if that happens we spin
// up a local browser helper to solve it and retry transparently.
func exchangeDiscordTicket(ctx context.Context, ticket, fingerprint string) (string, error) {
	body := map[string]any{"ticket": ticket}
	token, challenge, rawBody, err := postTicketExchange(ctx, fingerprint, body)
	if err != nil {
		return "", err
	}
	if challenge == nil {
		return token, nil
	}
	fmt.Println()
	fmt.Println("Discord requires a captcha solve. Opening helper in your browser...")
	solution, err := solveCaptchaInBrowser(ctx, challenge.Sitekey, challenge.RqData)
	if err != nil {
		return "", fmt.Errorf("captcha solve: %w", err)
	}
	body["captcha_key"] = solution
	body["captcha_rqtoken"] = challenge.RqToken
	token, challenge, rawBody, err = postTicketExchange(ctx, fingerprint, body)
	if err != nil {
		return "", err
	}
	if challenge != nil {
		return "", fmt.Errorf("captcha rejected — Discord still requires verification.\n"+
			"  Response body: %s\n"+
			"  This usually means the hCaptcha solution didn't bind to rqdata correctly.",
			string(rawBody))
	}
	return token, nil
}

type captchaChallenge struct {
	Sitekey string
	RqData  string
	RqToken string
	Service string
}

// postTicketExchange returns (token, challenge, rawBody, err).
//   - On success: token is set; challenge is nil; rawBody is the 200 body.
//   - On captcha-required: challenge is set with the fields Discord returned;
//     rawBody is the full 400 body for diagnostics.
//   - On any other error: err is set.
func postTicketExchange(ctx context.Context, fingerprint string, body map[string]any) (string, *captchaChallenge, []byte, error) {
	buf, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST",
		"https://discord.com/api/v9/users/@me/remote-auth/login",
		bytes.NewReader(buf),
	)
	if err != nil {
		return "", nil, nil, err
	}
	discordBrowserHeaders(req, fingerprint)

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", nil, nil, err
	}
	defer resp.Body.Close()
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))

	if resp.StatusCode == 200 {
		var out struct {
			EncryptedToken string `json:"encrypted_token"`
		}
		if err := json.Unmarshal(bodyBytes, &out); err != nil {
			return "", nil, bodyBytes, err
		}
		if out.EncryptedToken == "" {
			return "", nil, bodyBytes, fmt.Errorf("ticket exchange returned no encrypted_token")
		}
		return out.EncryptedToken, nil, bodyBytes, nil
	}

	if resp.StatusCode == 400 {
		var c struct {
			CaptchaKey     []string `json:"captcha_key"`
			CaptchaSitekey string   `json:"captcha_sitekey"`
			CaptchaService string   `json:"captcha_service"`
			CaptchaRqdata  string   `json:"captcha_rqdata"`
			CaptchaRqtoken string   `json:"captcha_rqtoken"`
		}
		if err := json.Unmarshal(bodyBytes, &c); err == nil && len(c.CaptchaKey) > 0 {
			return "", &captchaChallenge{
				Sitekey: c.CaptchaSitekey,
				RqData:  c.CaptchaRqdata,
				RqToken: c.CaptchaRqtoken,
				Service: c.CaptchaService,
			}, bodyBytes, nil
		}
	}
	return "", nil, bodyBytes, fmt.Errorf("ticket exchange %s: %s", resp.Status, string(bodyBytes))
}

// discordBrowserHeaders sets the full battery of headers a real Chrome client
// sends. Discord's anti-abuse system is much less likely to throw captcha at
// requests that look like they came from the actual web client.
func discordBrowserHeaders(req *http.Request, fingerprint string) {
	req.Header.Set("User-Agent", discordUserAgent)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", discordOrigin)
	req.Header.Set("Referer", "https://discord.com/login")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("X-Discord-Locale", "en-US")
	req.Header.Set("X-Discord-Timezone", "America/Los_Angeles")
	req.Header.Set("X-Super-Properties", discordSuperProperties())
	req.Header.Set("X-Debug-Options", "bugReporterEnabled")
	if fingerprint != "" {
		req.Header.Set("X-Fingerprint", fingerprint)
	}
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
}

// discordSuperProperties matches the X-Super-Properties Discord's web client
// sends. The client_build_number drifts over time; we hardcode a recent one,
// which is fine because Discord doesn't strictly enforce its freshness.
func discordSuperProperties() string {
	props := map[string]any{
		"os":                       "Mac OS X",
		"browser":                  "Chrome",
		"device":                   "",
		"system_locale":            "en-US",
		"browser_user_agent":       discordUserAgent,
		"browser_version":          "131.0.0.0",
		"os_version":               "10.15.7",
		"referrer":                 "",
		"referring_domain":         "",
		"referrer_current":         "",
		"referring_domain_current": "",
		"release_channel":          "stable",
		"client_build_number":      363081,
		"client_event_source":      nil,
	}
	buf, _ := json.Marshal(props)
	return base64.StdEncoding.EncodeToString(buf)
}

// fetchDiscordFingerprint asks /api/v9/experiments for a fingerprint Discord
// will accept on subsequent calls. Best-effort: if it fails, callers proceed
// without one (which is what the Python version does).
func fetchDiscordFingerprint(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET",
		"https://discord.com/api/v9/experiments", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", discordUserAgent)
	req.Header.Set("Origin", discordOrigin)
	req.Header.Set("Referer", "https://discord.com/login")
	req.Header.Set("X-Discord-Locale", "en-US")
	req.Header.Set("X-Super-Properties", discordSuperProperties())
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("experiments returned %s", resp.Status)
	}
	var out struct {
		Fingerprint string `json:"fingerprint"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Fingerprint, nil
}

// captchaHelperHTML uses explicit hcaptcha.render() so rqdata is reliably
// passed as widget config (rather than via the data-rqdata attribute, which
// some hCaptcha builds silently ignore). The solve is then bound to rqdata
// inside hCaptcha's verification API, and the resulting token will validate
// against Discord's captcha_rqtoken on retry.
const captchaHelperHTML = `<!DOCTYPE html>
<html>
<head>
  <title>Discord captcha — cass</title>
  <script src="https://js.hcaptcha.com/1/api.js?render=explicit&onload=onHCaptchaReady" async defer></script>
  <style>
    body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif; padding: 60px 40px; text-align: center; background: #f6f6f6; color: #2e3338; }
    h2 { color: #5865f2; margin-top: 0; }
    #captcha-container { display: inline-block; margin: 30px 0; }
    #status { margin-top: 24px; min-height: 1.2em; color: #4f545c; }
    .ok { color: #2d7d46; font-weight: 600; }
    .err { color: #b14a4a; }
    code { background: #ebebeb; padding: 2px 6px; border-radius: 3px; font-size: 12px; }
  </style>
</head>
<body>
  <h2>Discord login — captcha challenge</h2>
  <p>Discord asked cass to verify before completing your QR login.<br>
  Solve the captcha below and you'll be redirected automatically.</p>
  <div id="captcha-container"></div>
  <div id="status"></div>
  <script>
    const SITEKEY = "%s";
    const RQDATA  = "%s";
    let widgetID = null;
    function onHCaptchaReady() {
      widgetID = hcaptcha.render("captcha-container", {
        sitekey: SITEKEY,
        rqdata:  RQDATA,
        callback: onSolved,
        "error-callback": function(err) {
          var s = document.getElementById("status");
          s.textContent = "hCaptcha error: " + err;
          s.className = "err";
        },
      });
      // Some hCaptcha builds ignore rqdata in render() config; execute()
      // with rqdata is the canonical way to bind it.
      try { hcaptcha.execute(widgetID, { rqdata: RQDATA }); } catch (e) {}
    }
    function onSolved(token) {
      var s = document.getElementById("status");
      s.textContent = "Submitting…";
      fetch("/solved", {
        method: "POST",
        headers: {"Content-Type": "application/json"},
        body: JSON.stringify({token: token})
      }).then(function(r) { return r.text(); }).then(function(t) {
        s.textContent = t;
        s.className = "ok";
      }).catch(function(e) {
        s.textContent = "Error: " + e;
        s.className = "err";
      });
    }
  </script>
</body>
</html>
`

func solveCaptchaInBrowser(ctx context.Context, sitekey, rqdata string) (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	port := listener.Addr().(*net.TCPAddr).Port

	resultCh := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, captchaHelperHTML, sitekey, rqdata)
	})
	mux.HandleFunc("/solved", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token == "" {
			http.Error(w, "missing token", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte("✓ captcha submitted — you can close this tab"))
		select {
		case resultCh <- body.Token:
		default:
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(listener) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	helperURL := fmt.Sprintf("http://localhost:%d/", port)
	fmt.Println("If your browser doesn't open automatically, paste this URL:")
	fmt.Println("  " + helperURL)
	openBrowser(helperURL)

	select {
	case token := <-resultCh:
		return token, nil
	case <-time.After(5 * time.Minute):
		return "", fmt.Errorf("timed out waiting for captcha solve (5 minutes)")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func pushDiscordToken(token string) error {
	creds, err := auth.Read()
	if err != nil {
		return fmt.Errorf("not logged in (run: cass login): %w", err)
	}
	body, _ := json.Marshal(map[string]string{"discord_token": token})
	url := config.PortalURL() + "/api/extension/credentials/discord-mcp"
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
		return fmt.Errorf("auth service %s: %s", resp.Status, string(buf))
	}
	return nil
}
