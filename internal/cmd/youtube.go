package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/Cassandras-Edge/cass/internal/auth"
	"github.com/Cassandras-Edge/cass/internal/config"
)

// youtubeCmd is a discoverable top-level wrapper over the yt-mcp cookie sync
// (`cass cookies sync yt-mcp`), mirroring `cass twitter` / `cass gmail`.
func youtubeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "youtube",
		Short: "YouTube helper commands (link cookies for transcription)",
	}
	cmd.AddCommand(youtubeLinkCmd())
	cmd.AddCommand(youtubeStatusCmd())
	return cmd
}

func youtubeLinkCmd() *cobra.Command {
	var noOpen bool
	cmd := &cobra.Command{
		Use:   "link",
		Short: "Extract YouTube cookies from Firefox and push them to auth",
		RunE: func(_ *cobra.Command, _ []string) error {
			svc := cookieServices["yt-mcp"]
			fmt.Printf("── %s: %s ──\n", svc.Name, svc.Description)
			syncOneService(svc, false, noOpen)
			return nil
		},
	}
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "Don't open the login page on missing cookies")
	return cmd
}

func youtubeStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether YouTube cookies are linked",
		RunE: func(_ *cobra.Command, _ []string) error {
			creds, err := auth.Read()
			if err != nil {
				return fmt.Errorf("not logged in (run: cass login): %w", err)
			}
			u := fmt.Sprintf("%s/api/extension/credentials/yt-mcp", config.PortalURL())
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
				fmt.Println("YouTube: NOT LINKED")
				fmt.Println("  Run: cass youtube link")
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
			if s, _ := raw["youtube_cookies"].(string); s == "" {
				notLinked()
				return nil
			}
			fmt.Println("YouTube: LINKED")
			return nil
		},
	}
}
