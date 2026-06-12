package cmd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// titCmd — mint and manage one-time Tailscale device-enrollment keys for
// "tit" appliances. The long-lived OAuth client secret stays on this machine
// (env/tit.env in the stack repo); appliances only ever see short-lived,
// single-use tskey-auth keys.
func titCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tit",
		Short: "Tailscale enrollment keys for tit appliances",
	}
	cmd.AddCommand(titKeyCmd())
	cmd.AddCommand(titKeysCmd())
	cmd.AddCommand(titRevokeCmd())
	return cmd
}

const tsAPIBase = "https://api.tailscale.com/api/v2"

// titOAuthSecret resolves the Tailscale OAuth client secret:
// --oauth-secret flag, then TIT_OAUTH_SECRET env, then TS_AUTHKEY= in the
// stack repo's env/tit.env.
func titOAuthSecret(flagVal string) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	if v := os.Getenv("TIT_OAUTH_SECRET"); v != "" {
		return v, nil
	}
	home, _ := os.UserHomeDir()
	envFile := filepath.Join(home, "cassandra-stack", "env", "tit.env")
	f, err := os.Open(envFile)
	if err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if v, ok := strings.CutPrefix(line, "TS_AUTHKEY="); ok {
				return strings.Trim(v, `"'`), nil
			}
		}
	}
	return "", fmt.Errorf("no Tailscale OAuth secret: pass --oauth-secret, set TIT_OAUTH_SECRET, or put TS_AUTHKEY=tskey-client-... in %s", envFile)
}

// titAccessToken exchanges the OAuth client secret for a short-lived access
// token. Tailscale embeds the client ID in the secret itself
// (tskey-client-<ID>-<rest>); we send id+secret as form fields, and fall back
// to secret-only if that parse is rejected.
func titAccessToken(secret string) (string, error) {
	clientID := ""
	if rest, ok := strings.CutPrefix(secret, "tskey-client-"); ok {
		if i := strings.IndexByte(rest, '-'); i > 0 {
			clientID = rest[:i]
		}
	}
	tok, err := titTokenRequest(clientID, secret)
	if err != nil && clientID != "" {
		tok, err = titTokenRequest("", secret)
	}
	return tok, err
}

func titTokenRequest(clientID, secret string) (string, error) {
	form := url.Values{"client_id": {clientID}, "client_secret": {secret}}
	resp, err := http.PostForm(tsAPIBase+"/oauth/token", form)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("oauth token exchange failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.AccessToken == "" {
		return "", fmt.Errorf("oauth token exchange: unexpected response: %s", strings.TrimSpace(string(body)))
	}
	return out.AccessToken, nil
}

func titAPI(token, method, path string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, tsAPIBase+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s failed (%d): %s", method, path, resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func titKeyCmd() *cobra.Command {
	var oauthSecret, name string
	var tags []string
	var expiry time.Duration
	var reusable bool
	cmd := &cobra.Command{
		Use:   "key",
		Short: "Mint a one-time tagged enrollment key (prints only the key to stdout)",
		RunE: func(_ *cobra.Command, _ []string) error {
			secret, err := titOAuthSecret(oauthSecret)
			if err != nil {
				return err
			}
			token, err := titAccessToken(secret)
			if err != nil {
				return err
			}
			desc := name
			if desc == "" {
				desc, _ = os.Hostname()
			}
			payload := map[string]any{
				"capabilities": map[string]any{
					"devices": map[string]any{
						"create": map[string]any{
							"reusable":      reusable,
							"ephemeral":     false,
							"preauthorized": true,
							"tags":          tags,
						},
					},
				},
				"expirySeconds": int(expiry.Seconds()),
				"description":   "tit enrollment " + desc,
			}
			body, err := titAPI(token, http.MethodPost, "/tailnet/-/keys", payload)
			if err != nil {
				return err
			}
			var out struct {
				Key     string    `json:"key"`
				Expires time.Time `json:"expires"`
			}
			if err := json.Unmarshal(body, &out); err != nil || out.Key == "" {
				return fmt.Errorf("key mint: unexpected response: %s", strings.TrimSpace(string(body)))
			}
			// Key alone on stdout so it pipes cleanly; niceties on stderr.
			fmt.Println(out.Key)
			fmt.Fprintf(os.Stderr, "tags=%s expires=%s reusable=%v\n",
				strings.Join(tags, ","), out.Expires.Local().Format(time.RFC3339), reusable)
			return nil
		},
	}
	cmd.Flags().StringVar(&oauthSecret, "oauth-secret", "", "Tailscale OAuth client secret (default: TIT_OAUTH_SECRET, then env/tit.env)")
	cmd.Flags().StringVar(&name, "name", "", "Description suffix (default: this machine's hostname)")
	cmd.Flags().DurationVar(&expiry, "expiry", time.Hour, "Key lifetime")
	cmd.Flags().StringSliceVar(&tags, "tags", []string{"tag:tit"}, "ACL tags for the enrolled device")
	cmd.Flags().BoolVar(&reusable, "reusable", false, "Allow the key to enroll more than one device")
	return cmd
}

func titKeysCmd() *cobra.Command {
	var oauthSecret string
	cmd := &cobra.Command{
		Use:   "keys",
		Short: "List auth keys on the tailnet",
		RunE: func(_ *cobra.Command, _ []string) error {
			secret, err := titOAuthSecret(oauthSecret)
			if err != nil {
				return err
			}
			token, err := titAccessToken(secret)
			if err != nil {
				return err
			}
			body, err := titAPI(token, http.MethodGet, "/tailnet/-/keys", nil)
			if err != nil {
				return err
			}
			var out struct {
				Keys []struct {
					ID          string    `json:"id"`
					Description string    `json:"description"`
					Expires     time.Time `json:"expires"`
					Revoked     time.Time `json:"revoked"`
				} `json:"keys"`
			}
			if err := json.Unmarshal(body, &out); err != nil {
				return fmt.Errorf("list keys: unexpected response: %s", strings.TrimSpace(string(body)))
			}
			if len(out.Keys) == 0 {
				fmt.Println("No auth keys.")
				return nil
			}
			fmt.Println(tableHeaderStyle.Render(fmt.Sprintf("%-18s  %-34s  %-25s  %s", "ID", "DESCRIPTION", "EXPIRES", "REVOKED")))
			for _, k := range out.Keys {
				expires, revoked := "-", "-"
				if !k.Expires.IsZero() {
					expires = k.Expires.Local().Format(time.RFC3339)
				}
				if !k.Revoked.IsZero() {
					revoked = k.Revoked.Local().Format(time.RFC3339)
				}
				desc := k.Description
				if len(desc) > 34 {
					desc = desc[:31] + "..."
				}
				fmt.Printf("%-18s  %-34s  %-25s  %s\n", k.ID, desc, expires, revoked)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&oauthSecret, "oauth-secret", "", "Tailscale OAuth client secret (default: TIT_OAUTH_SECRET, then env/tit.env)")
	return cmd
}

func titRevokeCmd() *cobra.Command {
	var oauthSecret string
	cmd := &cobra.Command{
		Use:   "revoke <key-id>",
		Short: "Revoke an auth key",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			secret, err := titOAuthSecret(oauthSecret)
			if err != nil {
				return err
			}
			token, err := titAccessToken(secret)
			if err != nil {
				return err
			}
			if _, err := titAPI(token, http.MethodDelete, "/tailnet/-/keys/"+url.PathEscape(args[0]), nil); err != nil {
				return err
			}
			fmt.Printf("Revoked %s.\n", args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&oauthSecret, "oauth-secret", "", "Tailscale OAuth client secret (default: TIT_OAUTH_SECRET, then env/tit.env)")
	return cmd
}
