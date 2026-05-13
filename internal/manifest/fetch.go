package manifest

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// CacheTTL is how long a cached manifest stays fresh. After this, the next
// non-force fetch goes back to GitHub. 1 hour balances "see new manifests
// quickly" against "don't hammer the API on every Claude session start."
const CacheTTL = 1 * time.Hour

// Bundle is the manifest + the raw skill markdown for a single service.
type Bundle struct {
	Manifest Manifest
	Skill    string // raw markdown body; empty if no skill
}

// Fetch returns the Bundle for the named service. Uses cache when fresh
// unless force=true.
//
// On any failure (network, missing manifest, gh auth) returns a wrapped
// error — the caller decides whether to fall back to cache, skip the
// service, or surface the error.
func Fetch(repo, service string, force bool) (*Bundle, error) {
	cachePath, err := cachePathFor(service)
	if err != nil {
		return nil, err
	}
	if !force {
		if bundle, ok := loadFromCache(cachePath); ok {
			return bundle, nil
		}
	}

	man, err := fetchFile(repo, "cass-manifest.json")
	if err != nil {
		// On fetch failure, fall back to stale cache if we have one — better
		// to keep a service installed than to wipe it on a transient
		// network blip.
		if stale, ok := loadStale(cachePath); ok {
			return stale, fmt.Errorf("using stale manifest for %s (fetch failed: %w)", service, err)
		}
		return nil, fmt.Errorf("fetch manifest for %s: %w", service, err)
	}
	var m Manifest
	if err := json.Unmarshal(man, &m); err != nil {
		return nil, fmt.Errorf("decode manifest for %s: %w", service, err)
	}
	if m.Name == "" {
		return nil, fmt.Errorf("manifest for %s has no `name` field", service)
	}

	bundle := &Bundle{Manifest: m}
	if m.HasSkill() {
		skill, err := fetchFile(repo, m.SkillPath())
		if err != nil {
			// Skill is optional — log and continue rather than fail the
			// whole bundle. The caller decides whether to surface the
			// warning.
			bundle.Skill = ""
		} else {
			bundle.Skill = string(skill)
		}
	}

	saveToCache(cachePath, bundle)
	return bundle, nil
}

// fetchFile reads a single file from a GitHub repo via `gh api`. Works for
// private repos as long as the user is authenticated.
//
// We shell out to `gh` instead of hitting the API directly so the user's
// existing auth (keyring, env vars, config) is automatically respected.
func fetchFile(repo, path string) ([]byte, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return nil, errors.New("gh CLI not found in PATH. Install: brew install gh && gh auth login")
	}
	out, err := exec.Command("gh", "api", fmt.Sprintf("repos/%s/contents/%s", repo, path)).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("gh api: %s", string(ee.Stderr))
		}
		return nil, fmt.Errorf("gh api: %w", err)
	}
	var resp struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("decode gh response: %w", err)
	}
	if resp.Encoding != "base64" {
		return nil, fmt.Errorf("unexpected gh content encoding: %s", resp.Encoding)
	}
	return base64.StdEncoding.DecodeString(resp.Content)
}

// ── Cache ──────────────────────────────────────────────────────────────

func cacheDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".cache", "cass", "manifests")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

func cachePathFor(service string) (string, error) {
	dir, err := cacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, service+".json"), nil
}

type cacheEntry struct {
	FetchedAt time.Time `json:"fetched_at"`
	Manifest  Manifest  `json:"manifest"`
	Skill     string    `json:"skill"`
}

func loadFromCache(path string) (*Bundle, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, false
	}
	if time.Since(info.ModTime()) > CacheTTL {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var entry cacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, false
	}
	return &Bundle{Manifest: entry.Manifest, Skill: entry.Skill}, true
}

// loadStale ignores TTL and returns whatever's on disk. Used as fallback
// when a fresh fetch fails.
func loadStale(path string) (*Bundle, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var entry cacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, false
	}
	return &Bundle{Manifest: entry.Manifest, Skill: entry.Skill}, true
}

func saveToCache(path string, bundle *Bundle) {
	entry := cacheEntry{
		FetchedAt: time.Now(),
		Manifest:  bundle.Manifest,
		Skill:     bundle.Skill,
	}
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o644)
}
