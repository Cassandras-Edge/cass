package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/Cassandras-Edge/cass/internal/auth"
	"github.com/Cassandras-Edge/cass/internal/config"
)

func loginCmd() *cobra.Command {
	var device string
	var paste bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Authenticate with the Cassandra portal via browser",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLogin(cmd.Context(), device, paste)
		},
	}
	cmd.Flags().StringVar(&device, "device", "", "device name (defaults to hostname)")
	cmd.Flags().BoolVar(&paste, "paste", false, "Paste the redirect URL back into the terminal instead of relying on the localhost callback (auto-enabled on headless hosts)")
	return cmd
}

func runLogin(ctx context.Context, device string, paste bool) error {
	if device == "" {
		host, _ := os.Hostname()
		host, _, _ = strings.Cut(host, ".")
		device = host
	}
	if len(device) > 64 {
		device = device[:64]
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("bind localhost: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	resultCh := make(chan url.Values, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		params := r.URL.Query()
		if params.Get("key") == "" || params.Get("email") == "" {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, "<html><body><h2>Login failed</h2></body></html>")
			errCh <- errors.New("portal callback missing key/email")
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body><h2>Authenticated!</h2><p>You can close this tab and return to the terminal.</p><script>window.close()</script></body></html>`)
		resultCh <- params
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(listener) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	callbackURL := fmt.Sprintf("http://localhost:%d/callback", port)
	q := url.Values{}
	q.Set("callback", callbackURL)
	q.Set("device", device)
	loginURL := fmt.Sprintf("%s/api/cli/login?%s", config.PortalURL(), q.Encode())

	fmt.Printf("Opening browser for login (device: %s)...\n", device)
	fmt.Printf("If it doesn't open, visit: %s\n", loginURL)
	openBrowser(loginURL)

	// Auto-detect a headless host: with no local display the browser's redirect
	// to localhost can't come back here, so the callback would hang forever.
	// Switch to paste mode automatically (only when we have a TTY to read from).
	if !paste && isHeadless() {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			// Callback can't come back and there's no terminal to paste into —
			// fail fast instead of blocking on a callback that will never arrive.
			return errors.New("no local display and stdin is not a terminal — re-run interactively with --paste")
		}
		fmt.Println("\nNo local display detected — the browser can't redirect back to this host.")
		fmt.Println("Switching to paste mode.")
		paste = true
	}

	// In paste mode the browser's redirect to localhost can't reach this host
	// (headless box, remote browser). Read the redirect URL directly from the
	// terminal instead of waiting on the HTTP callback. This is synchronous on
	// the main goroutine — no background stdin reader to leak or to race the
	// interactive setup form for stdin.
	if paste {
		params, err := readPastedCallback()
		if err != nil {
			return err
		}
		return persistLogin(params, device)
	}

	select {
	case params := <-resultCh:
		return persistLogin(params, device)
	case err := <-errCh:
		return err
	case <-time.After(5 * time.Minute):
		return errors.New("timed out waiting for browser callback")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// readPastedCallback prompts for and reads a pasted redirect URL (or bare query
// string) from the terminal, with echo suppressed so the key/cf_client_secret
// it carries don't land in terminal scrollback or logs. Requires an interactive
// terminal — paste mode is meaningless without one.
func readPastedCallback() (url.Values, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return nil, errors.New("--paste needs an interactive terminal to read the pasted redirect URL")
	}
	fmt.Println()
	fmt.Println("Headless paste mode — after authenticating in the browser, copy the full")
	fmt.Println("redirect URL (starts with http://localhost:...) and paste it below.")
	fmt.Print("Redirect URL (input hidden): ")

	raw, err := term.ReadPassword(fd)
	fmt.Println()
	if err != nil {
		return nil, fmt.Errorf("read pasted callback: %w", err)
	}
	return parseCallbackInput(string(raw))
}

// parseCallbackInput accepts either a full redirect URL
// (http://localhost:PORT/callback?key=...&email=...) or just its query string
// and returns the parsed values, validating that key and email are present.
func parseCallbackInput(s string) (url.Values, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "?")
	if s == "" {
		return nil, errors.New("no callback URL pasted")
	}
	// If it parses as a URL with a query, use that; otherwise treat the whole
	// string as a bare query (user pasted only key=...&email=...).
	if u, perr := url.Parse(s); perr == nil && u.RawQuery != "" {
		s = u.RawQuery
	}
	vals, err := url.ParseQuery(s)
	if err != nil {
		return nil, fmt.Errorf("could not parse pasted callback: %w", err)
	}
	if vals.Get("key") == "" || vals.Get("email") == "" {
		return nil, errors.New("pasted callback missing key/email — copy the full localhost URL from the browser after authenticating")
	}
	return vals, nil
}

func persistLogin(p url.Values, device string) error {
	cid := p.Get("cf_client_id")
	csec := p.Get("cf_client_secret")
	if cid == "" || csec == "" {
		return errors.New("portal didn't return a CF service token (cf_client_id/secret missing — is PORTAL_ACCESS_APP_ID configured on portal?)")
	}
	devName := p.Get("device_name")
	if devName == "" {
		devName = device
	}
	creds := auth.DeviceCreds{
		Email:                p.Get("email"),
		DeviceName:           devName,
		CFAccessClientID:     cid,
		CFAccessClientSecret: csec,
		MCPKey:               p.Get("key"),
	}
	if err := auth.Write(creds); err != nil {
		return fmt.Errorf("write env file: %w", err)
	}
	added, _ := auth.EnsureZprofileSources()
	fmt.Printf("  Credentials → %s\n", auth.EnvPath())
	if added {
		fmt.Printf("  Added 'source %s' to ~/.zprofile\n", auth.EnvPath())
	}
	fmt.Printf("Logged in as %s\n", creds.Email)
	return nil
}

// isHeadless reports whether this host has no local display for the browser to
// redirect back to — a Linux box with no X11/Wayland session, which is the
// usual case over SSH. macOS and Windows are assumed to always have a GUI.
func isHeadless() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	return os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == ""
}

func openBrowser(u string) {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", u)
	case "linux":
		c = exec.Command("xdg-open", u)
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	}
	if c != nil {
		_ = c.Start()
	}
}
