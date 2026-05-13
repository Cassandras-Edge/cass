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

	"github.com/Cassandras-Edge/cass/internal/auth"
	"github.com/Cassandras-Edge/cass/internal/config"
)

func loginCmd() *cobra.Command {
	var device string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Authenticate with the Cassandra portal via browser",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLogin(cmd.Context(), device)
		},
	}
	cmd.Flags().StringVar(&device, "device", "", "device name (defaults to hostname)")
	return cmd
}

func runLogin(ctx context.Context, device string) error {
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
