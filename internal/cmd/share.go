package cmd

import (
	"bytes"
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
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Cassandras-Edge/cass/internal/auth"
	"github.com/Cassandras-Edge/cass/internal/share"
)

const shareURLDefault = "https://share.cassandrasedge.com"

func shareCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "share",
		Short: "Share Claude Code conversations via ephemeral URLs",
	}
	cmd.AddCommand(shareCreateCmd())
	cmd.AddCommand(shareListCmd())
	cmd.AddCommand(shareRevokeCmd())
	return cmd
}

func shareBaseURL() string {
	if u := os.Getenv("CASS_SHARE_URL"); u != "" {
		return u
	}
	return shareURLDefault
}

// shareRequest fires a request to the share service with CF Access service-token
// + Bearer headers from ~/.cass/env, then decodes the response.
func shareRequest(method, path string, body, out any) error {
	creds, err := auth.Read()
	if err != nil {
		return fmt.Errorf("not logged in (run: cass login): %w", err)
	}
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, shareBaseURL()+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("CF-Access-Client-Id", creds.CFAccessClientID)
	req.Header.Set("CF-Access-Client-Secret", creds.CFAccessClientSecret)
	req.Header.Set("Authorization", "Bearer "+creds.MCPKey)
	if devEmail := os.Getenv("CASS_DEV_EMAIL"); devEmail != "" {
		req.Header.Set("X-Dev-Email", devEmail)
	} else if creds.Email != "" {
		req.Header.Set("X-Dev-Email", creds.Email)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("share %s: %s", resp.Status, string(buf))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func shareCreateCmd() *cobra.Command {
	var ttl, title, summary string
	var once, noCopy bool
	cmd := &cobra.Command{
		Use:   "create [SESSION]",
		Short: "Upload the current (or specified) session as a share link",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			var sessionPath string
			if len(args) == 1 {
				sessionPath = args[0]
			}
			return runShareCreate(sessionPath, ttl, title, summary, once, noCopy)
		},
	}
	cmd.Flags().StringVar(&ttl, "ttl", "24h", "Expiry: e.g. 6h, 24h, 7d")
	cmd.Flags().BoolVar(&once, "once", false, "Self-destruct after first fetch")
	cmd.Flags().StringVar(&title, "title", "", "Optional human title")
	cmd.Flags().StringVar(&summary, "summary", "", "2-3 line summary; defaults to the first user turn")
	cmd.Flags().BoolVar(&noCopy, "no-copy", false, "Skip copying to clipboard")
	return cmd
}

func runShareCreate(sessionArg, ttl, title, summary string, once, noCopy bool) error {
	hours, err := parseTTL(ttl)
	if err != nil {
		return err
	}
	jsonlPath, err := resolveSessionPath(sessionArg)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Reading %s...\n", jsonlPath)
	f, err := os.Open(jsonlPath)
	if err != nil {
		return err
	}
	body := share.JSONLToMarkdown(f, title)
	f.Close()
	fmt.Fprintf(os.Stderr, "  → %d chars of sanitized markdown\n", len(body))

	if summary == "" {
		for _, line := range strings.Split(body, "\n") {
			s := strings.TrimSpace(line)
			if s == "" || strings.HasPrefix(s, "#") || strings.HasPrefix(s, "```") {
				continue
			}
			if len(s) > 200 {
				s = s[:200]
			}
			summary = s
			break
		}
		if summary == "" {
			summary = "(no summary)"
		}
	}

	payload := map[string]any{
		"body":      body,
		"title":     title,
		"summary":   summary,
		"ttl_hours": hours,
		"once":      once,
	}
	var resp struct {
		URL       string `json:"url"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := shareRequest("POST", "/share", payload, &resp); err != nil {
		return err
	}

	expiryNote := "expires " + resp.ExpiresAt
	if once {
		expiryNote += " — single-use"
	}
	clip := fmt.Sprintf("Continue this Claude convo (%s):\ncurl -sSL '%s'\n\nAbout: %s\n",
		expiryNote, resp.URL, summary)
	fmt.Println()
	fmt.Println(clip)
	if !noCopy && copyToClipboard(clip) {
		fmt.Fprintln(os.Stderr, "✔ copied to clipboard")
	} else if !noCopy {
		fmt.Fprintln(os.Stderr, "(clipboard tool unavailable — text above is the share)")
	}
	return nil
}

func shareListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List your active share links",
		RunE: func(_ *cobra.Command, _ []string) error {
			var shares []struct {
				Token     string `json:"token"`
				URL       string `json:"url"`
				ExpiresAt string `json:"expires_at"`
				Title     string `json:"title"`
				Once      bool   `json:"once"`
			}
			if err := shareRequest("GET", "/share", nil, &shares); err != nil {
				return err
			}
			if len(shares) == 0 {
				fmt.Println("(no active shares)")
				return nil
			}
			for _, s := range shares {
				once := ""
				if s.Once {
					once = " [once]"
				}
				fmt.Printf("%s%s  expires %s  %s\n", s.Token, once, s.ExpiresAt, s.Title)
				fmt.Printf("  %s\n", s.URL)
			}
			return nil
		},
	}
}

func shareRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <token>",
		Short: "Revoke a share link early",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := shareRequest("DELETE", "/share/"+args[0], nil, nil); err != nil {
				return err
			}
			fmt.Printf("Revoked %s\n", args[0])
			return nil
		},
	}
}

// resolveSessionPath finds the JSONL to share. Precedence: explicit arg,
// $CLAUDE_SESSION_ID lookup under ~/.claude/projects, or newest .jsonl in
// the cwd-hashed project dir.
func resolveSessionPath(arg string) (string, error) {
	if arg != "" {
		expanded, err := expandHome(arg)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(expanded); err != nil {
			return "", fmt.Errorf("no such file: %s", expanded)
		}
		return expanded, nil
	}
	home, _ := os.UserHomeDir()
	base := filepath.Join(home, ".claude", "projects")
	if _, err := os.Stat(base); err != nil {
		return "", fmt.Errorf("%s missing — Claude Code not set up on this machine", base)
	}
	if sid := os.Getenv("CLAUDE_SESSION_ID"); sid != "" {
		entries, _ := os.ReadDir(base)
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			candidate := filepath.Join(base, e.Name(), sid+".jsonl")
			if _, err := os.Stat(candidate); err == nil {
				return candidate, nil
			}
		}
	}
	// Fallback: newest .jsonl in the cwd-hashed dir.
	cwd, _ := os.Getwd()
	mangled := "-" + strings.ReplaceAll(cwd, "/", "-")
	dir := filepath.Join(base, mangled)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("could not locate session .jsonl; pass the path explicitly")
	}
	type fileMod struct {
		path string
		mod  time.Time
	}
	var files []fileMod
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, fileMod{filepath.Join(dir, e.Name()), info.ModTime()})
	}
	if len(files) == 0 {
		return "", fmt.Errorf("could not locate session .jsonl; pass the path explicitly")
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	return files[0].path, nil
}

func expandHome(p string) (string, error) {
	if !strings.HasPrefix(p, "~") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, p[1:]), nil
}

var ttlPattern = regexp.MustCompile(`^(\d+)([hd])$`)

func parseTTL(ttl string) (int, error) {
	m := ttlPattern.FindStringSubmatch(strings.ToLower(strings.TrimSpace(ttl)))
	if m == nil {
		return 0, fmt.Errorf("invalid --ttl %q; use forms like 6h, 24h, 7d", ttl)
	}
	n, _ := strconv.Atoi(m[1])
	if m[2] == "h" {
		return n, nil
	}
	return n * 24, nil
}

func copyToClipboard(s string) bool {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("pbcopy")
	case "linux":
		if _, err := exec.LookPath("xclip"); err == nil {
			c = exec.Command("xclip", "-selection", "clipboard")
		} else if _, err := exec.LookPath("wl-copy"); err == nil {
			c = exec.Command("wl-copy")
		}
	}
	if c == nil {
		return false
	}
	c.Stdin = strings.NewReader(s)
	return c.Run() == nil
}
