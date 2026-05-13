package cmd

import (
	"bytes"
	"encoding/json"
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

	"github.com/spf13/cobra"

	"github.com/Cassandras-Edge/cass/internal/config"
)

const (
	twitterSharedKey = "twitter-mcp-queryids"
	twitterUA        = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:135.0) Gecko/20100101 Firefox/135.0"
)

var (
	bundleURLPattern = regexp.MustCompile(`(?:src|href)=["'](https://abs\.twimg\.com/responsive-web/client-web[^"']+\.js)["']`)
	opPattern        = regexp.MustCompile(`queryId:\s*"([A-Za-z0-9_-]+)"[^}]{0,200}operationName:\s*"([^"]+)"`)
)

func twitterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "twitter",
		Short: "X / Twitter helper commands",
	}
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
			if err := pushTwitterQueryIDs(ids); err != nil {
				return err
			}
			fmt.Printf("Pushed to /service-credentials/%s ✓\n", twitterSharedKey)
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

func pushTwitterQueryIDs(ids map[string]string) error {
	secret := config.AuthSecret()
	if secret == "" {
		return fmt.Errorf("AUTH_SECRET not set — service-credentials writes need the shared admin secret (env/acl.env)")
	}
	body, _ := json.Marshal(map[string]any{"queryIds": ids})
	req, _ := http.NewRequest("POST", config.AuthURL()+"/service-credentials/"+twitterSharedKey, bytes.NewReader(body))
	req.Header.Set("X-Auth-Secret", secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("auth %s: %s", resp.Status, string(buf))
	}
	return nil
}
