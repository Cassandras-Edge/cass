package portal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Cassandras-Edge/cass/internal/auth"
	"github.com/Cassandras-Edge/cass/internal/config"
)

// Client wraps an http.Client preloaded with CF Access service-token + Bearer
// headers from ~/.cass/env. One per process is plenty.
type Client struct {
	creds   auth.DeviceCreds
	baseURL string
	http    *http.Client
}

func NewClient() (*Client, error) {
	creds, err := auth.Read()
	if err != nil {
		return nil, fmt.Errorf("not logged in (run: cass login): %w", err)
	}
	return &Client{
		creds:   creds,
		baseURL: config.PortalURL(),
		http:    &http.Client{Timeout: 15 * time.Second},
	}, nil
}

func (c *Client) do(method, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, c.baseURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("CF-Access-Client-Id", c.creds.CFAccessClientID)
	req.Header.Set("CF-Access-Client-Secret", c.creds.CFAccessClientSecret)
	req.Header.Set("Authorization", "Bearer "+c.creds.MCPKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.http.Do(req)
}

func (c *Client) Email() string { return c.creds.Email }

// WhoamiResponse is the portal's /api/extension/whoami response. expires_at
// is included when the request was authenticated with Bearer mcp_… and the
// key has an expiry recorded.
type WhoamiResponse struct {
	Email     string `json:"email"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// Whoami returns the portal's view of this device's identity. Used to check
// MCP key expiry without needing AUTH_SECRET.
func (c *Client) Whoami() (*WhoamiResponse, error) {
	var out WhoamiResponse
	if err := c.Get("/api/extension/whoami", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// WhoamiWithKey hits whoami using a specific Bearer key instead of the
// default one in ~/.cass/env. Used by refresh-keys to check per-service
// keys' expiry. Returns (nil, ErrKeyNotValid) if the key is rejected.
func (c *Client) WhoamiWithKey(key string) (*WhoamiResponse, error) {
	req, err := http.NewRequest("GET", c.baseURL+"/api/extension/whoami", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("CF-Access-Client-Id", c.creds.CFAccessClientID)
	req.Header.Set("CF-Access-Client-Secret", c.creds.CFAccessClientSecret)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, ErrKeyNotValid
	}
	var out WhoamiResponse
	if err := decodeOrError(resp, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ErrKeyNotValid is returned by WhoamiWithKey when the portal rejected the
// supplied key — indicates the key is revoked/expired and should be re-minted.
var ErrKeyNotValid = errors.New("portal rejected the key (revoked or expired)")

// ValidateKey hits POST /api/keys/validate to confirm the key is still alive
// in the auth service's DB. Returns:
//   - true:  key is valid OR we couldn't determine (transient error). Caller
//     should keep using the cached key.
//   - false: portal definitively says the key is invalid; caller should
//     re-mint.
//
// This mirrors Python's `_key_is_alive` semantics — don't thrash new keys on
// transient network blips, only on authoritative "not valid" responses.
func (c *Client) ValidateKey(key string) bool {
	resp, err := c.do("POST", "/api/keys/validate", map[string]string{"key": key})
	if err != nil {
		return true
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return true
	}
	var out struct {
		Valid bool `json:"valid"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return true
	}
	return out.Valid
}

// Get unmarshals a successful GET response into out.
func (c *Client) Get(path string, out any) error {
	resp, err := c.do("GET", path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decodeOrError(resp, out)
}

// Post sends body, unmarshals successful response into out (may be nil).
func (c *Client) Post(path string, body, out any) error {
	resp, err := c.do("POST", path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decodeOrError(resp, out)
}

// Delete returns the status code, errors only on transport failure.
func (c *Client) Delete(path string) (int, error) {
	resp, err := c.do("DELETE", path, nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

func decodeOrError(resp *http.Response, out any) error {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("portal %s: %s", resp.Status, string(body))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
