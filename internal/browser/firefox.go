// Package browser provides direct readers for browser cookie stores. Used by
// `cass cookies status` and `cass cookies refresh` to avoid shelling to yt-dlp
// for fast read-only operations.
package browser

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/golang/snappy"
	_ "modernc.org/sqlite"
)

// Cookie is a single cookie read from Firefox's moz_cookies table. Firefox
// stores cookie values in plaintext, so no decryption is needed.
type Cookie struct {
	Host              string
	Name              string
	Value             string
	Path              string
	Expiry            int64
	Secure            bool
	IncludeSubdomains bool
}

// NetscapeLine emits one tab-separated Netscape cookie-jar line exactly as
// yt-dlp/curl expect: 7 tab-separated fields —
// host, includeSubdomains, path, secure, expiry, name, value.
func (c Cookie) NetscapeLine() string {
	bflag := func(b bool) string {
		if b {
			return "TRUE"
		}
		return "FALSE"
	}
	path := c.Path
	if path == "" {
		path = "/" // Netscape jars need a path field; yt-dlp emits "/" too.
	}
	return strings.Join([]string{
		c.Host,
		bflag(c.IncludeSubdomains),
		path,
		bflag(c.Secure),
		strconv.FormatInt(c.Expiry, 10),
		c.Name,
		c.Value,
	}, "\t")
}

// ReadFirefoxCookies returns all non-expired cookies whose host matches any of
// `domains` using the same rule the cookies command uses elsewhere
// (host == d || strings.HasSuffix(host, d)). Reads directly from Firefox's
// plaintext moz_cookies — no yt-dlp, no decryption.
func ReadFirefoxCookies(domains []string) ([]Cookie, error) {
	db, tmp, schemaVer, err := openFirefoxCopy()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	defer os.Remove(tmp)

	// FF142+ (schema >= 16) stores expiry in ms; compare against a ms threshold
	// in SQL, then normalize each expiry to seconds below. expiry == 0 means a
	// session cookie and is left untouched.
	now := expiryNowThreshold(schemaVer)
	rows, err := db.Query(
		"SELECT host, name, value, path, expiry, isSecure FROM moz_cookies " +
			"WHERE expiry = 0 OR expiry > " + strconv.FormatInt(now, 10))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Cookie
	for rows.Next() {
		var (
			host, name, value, path string
			expiry                  int64
			isSecure                int
		)
		if err := rows.Scan(&host, &name, &value, &path, &expiry, &isSecure); err != nil {
			return nil, err
		}
		matched := false
		for _, d := range domains {
			if host == d || strings.HasSuffix(host, d) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		out = append(out, Cookie{
			Host:              host,
			Name:              name,
			Value:             value,
			Path:              path,
			Expiry:            normalizeExpiry(expiry, schemaVer),
			Secure:            isSecure != 0,
			IncludeSubdomains: strings.HasPrefix(host, "."),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// normalizeExpiry converts a raw moz_cookies.expiry to Unix seconds. Firefox
// 142+ (cookies DB schema user_version >= 16) stores expiry in MILLISECONDS;
// older schemas store seconds. Session cookies (expiry == 0) are left as-is.
// Mirrors yt-dlp's yt_dlp/cookies.py: `if db_schema_version >= 16 and expiry
// is not None: expiry /= 1000`.
func normalizeExpiry(expiry int64, schemaVer int) int64 {
	if schemaVer >= 16 && expiry != 0 {
		return expiry / 1000
	}
	return expiry
}

// expiryNowThreshold returns the current time in the same unit moz_cookies.expiry
// uses for the given schema version: milliseconds for FF142+ (schema >= 16),
// seconds otherwise. Used in SQL WHERE clauses that compare against raw expiry.
func expiryNowThreshold(schemaVer int) int64 {
	now := time.Now().Unix()
	if schemaVer >= 16 {
		return now * 1000
	}
	return now
}

// firefoxSchemaVersion reads PRAGMA user_version from an open moz_cookies DB.
// Returns 0 on any error (treated as a pre-FF142 seconds-based schema).
func firefoxSchemaVersion(db *sql.DB) int {
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return 0
	}
	return v
}

// FindFirefoxCookiesDB returns the path to the default Firefox profile's
// cookies.sqlite, or "" if Firefox isn't installed / no profile found.
// macOS-only for now; Linux/Windows fallbacks are easy to add but not needed
// by the calling commands.
func FindFirefoxCookiesDB() string {
	home, _ := os.UserHomeDir()
	var pattern string
	switch runtime.GOOS {
	case "darwin":
		pattern = filepath.Join(home, "Library/Application Support/Firefox/Profiles/*/cookies.sqlite")
	case "linux":
		pattern = filepath.Join(home, ".mozilla/firefox/*/cookies.sqlite")
	default:
		return ""
	}
	matches, _ := filepath.Glob(pattern)
	if len(matches) == 0 {
		return ""
	}
	return matches[0]
}

// openFirefoxCopy copies the cookies.sqlite to a temp file (Firefox may be
// running and holding a WAL lock) and opens it read-only. Also reads the DB
// schema version (PRAGMA user_version) so callers can normalize the expiry
// unit: FF142+ (schema >= 16) stores expiry in milliseconds, older schemas in
// seconds. Caller must close the returned DB and remove the temp file.
func openFirefoxCopy() (*sql.DB, string, int, error) {
	src := FindFirefoxCookiesDB()
	if src == "" {
		return nil, "", 0, fmt.Errorf("Firefox cookies.sqlite not found")
	}
	tmp, err := os.CreateTemp("", "cass-fox-*.sqlite")
	if err != nil {
		return nil, "", 0, err
	}
	tmpPath := tmp.Name()
	sf, err := os.Open(src)
	if err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return nil, "", 0, err
	}
	if _, err := io.Copy(tmp, sf); err != nil {
		sf.Close()
		tmp.Close()
		os.Remove(tmpPath)
		return nil, "", 0, err
	}
	sf.Close()
	tmp.Close()
	db, err := sql.Open("sqlite", tmpPath+"?mode=ro")
	if err != nil {
		os.Remove(tmpPath)
		return nil, "", 0, err
	}
	return db, tmpPath, firefoxSchemaVersion(db), nil
}

// HasFirefoxCookies returns true iff Firefox holds at least one non-expired
// cookie matching `domains`. When `requiredNames` is non-empty, ALL of those
// cookie names must be present (and non-expired) on the matching hosts.
// Fast: typical query is <5 ms after the SQLite copy.
func HasFirefoxCookies(domains []string, requiredNames []string) bool {
	db, tmp, schemaVer, err := openFirefoxCopy()
	if err != nil {
		return false
	}
	defer db.Close()
	defer os.Remove(tmp)

	// Compare against a threshold in the same unit as the raw expiry column
	// (ms for FF142+/schema >= 16, seconds otherwise).
	now := expiryNowThreshold(schemaVer)
	args := []any{}
	placeholders := ""
	for i, d := range domains {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
		args = append(args, d)
	}

	if len(requiredNames) > 0 {
		namePH := ""
		for i, n := range requiredNames {
			if i > 0 {
				namePH += ","
			}
			namePH += "?"
			args = append(args, n)
		}
		args = append(args, now)
		q := fmt.Sprintf(
			"SELECT COUNT(DISTINCT name) FROM moz_cookies "+
				"WHERE host IN (%s) AND name IN (%s) "+
				"AND (expiry = 0 OR expiry > ?)",
			placeholders, namePH)
		var n int
		if err := db.QueryRow(q, args...).Scan(&n); err != nil {
			return false
		}
		return n == len(requiredNames)
	}

	args = append(args, now)
	q := fmt.Sprintf(
		"SELECT COUNT(*) FROM moz_cookies "+
			"WHERE host IN (%s) AND (expiry = 0 OR expiry > ?)",
		placeholders)
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// LSNGStore is one Firefox LocalStorage Next-Gen (LSNG) origin store, read from
// a profile's storage/default/<origin>/ls/data.sqlite. Values are decompressed.
type LSNGStore struct {
	Profile string            // profile directory name (e.g. "jr3v8x3r.default-release")
	Path    string            // the data.sqlite path
	Values  map[string]string // localStorage key -> value (Snappy-decompressed)
}

// ReadFirefoxLSNG reads localStorage for `originDir` (Firefox's escaped origin
// directory name, e.g. "https+++web.plaud.ai") across every Firefox profile,
// optionally filtered by a profile-name substring. Modern Firefox keeps
// localStorage in the per-origin LSNG store (storage/default/<origin>/ls/
// data.sqlite) — NOT the legacy webappsstore.sqlite — and Snappy-compresses
// values whose compression_type == 1. Returns one LSNGStore per profile that
// has the origin.
func ReadFirefoxLSNG(originDir, profileFilter string) ([]LSNGStore, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	var base string
	switch runtime.GOOS {
	case "darwin":
		base = filepath.Join(home, "Library", "Application Support", "Firefox", "Profiles")
	case "linux":
		base = filepath.Join(home, ".mozilla", "firefox")
	case "windows":
		base = filepath.Join(os.Getenv("APPDATA"), "Mozilla", "Firefox", "Profiles")
	default:
		return nil, fmt.Errorf("unsupported OS for Firefox LSNG: %s", runtime.GOOS)
	}
	profiles, _ := os.ReadDir(base)
	var stores []LSNGStore
	for _, p := range profiles {
		if !p.IsDir() {
			continue
		}
		if profileFilter != "" && !strings.Contains(strings.ToLower(p.Name()), strings.ToLower(profileFilter)) {
			continue
		}
		src := filepath.Join(base, p.Name(), "storage", "default", originDir, "ls", "data.sqlite")
		if _, err := os.Stat(src); err != nil {
			continue
		}
		vals, err := readLSNGFile(src)
		if err != nil || len(vals) == 0 {
			continue
		}
		stores = append(stores, LSNGStore{Profile: p.Name(), Path: src, Values: vals})
	}
	return stores, nil
}

// readLSNGFile opens a copy of a LSNG data.sqlite and returns its decompressed
// key/value pairs. compression_type == 1 means the value is Snappy block-format
// compressed (same as yt-dlp / Firefox source); 0 is stored verbatim.
func readLSNGFile(src string) (map[string]string, error) {
	tmp, err := os.CreateTemp("", "cass-fox-ls-*.sqlite")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	sf, err := os.Open(src)
	if err != nil {
		tmp.Close()
		return nil, err
	}
	if _, err := io.Copy(tmp, sf); err != nil {
		sf.Close()
		tmp.Close()
		return nil, err
	}
	sf.Close()
	tmp.Close()
	db, err := sql.Open("sqlite", tmpPath+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query("SELECT key, compression_type, value FROM data")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var (
			key      string
			compType int
			value    []byte
		)
		if err := rows.Scan(&key, &compType, &value); err != nil {
			return nil, err
		}
		if compType == 1 {
			dec, err := snappy.Decode(nil, value)
			if err != nil {
				continue // skip anything we can't decompress rather than failing the whole read
			}
			value = dec
		}
		out[key] = string(value)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ReadFirefoxCookie returns (value, expiryUnix) for the first cookie matching
// hostLike (SQL LIKE pattern, e.g. "%perplexity.ai") + name, or empty/0 if
// not found. Used by `cookies refresh` to poll cf_clearance rotation.
func ReadFirefoxCookie(hostLike, name string) (string, int64) {
	db, tmp, schemaVer, err := openFirefoxCopy()
	if err != nil {
		return "", 0
	}
	defer db.Close()
	defer os.Remove(tmp)
	var (
		value  string
		expiry int64
	)
	row := db.QueryRow(
		"SELECT value, expiry FROM moz_cookies WHERE host LIKE ? AND name = ? LIMIT 1",
		hostLike, name)
	if err := row.Scan(&value, &expiry); err != nil {
		return "", 0
	}
	// Normalize FF142+ (schema >= 16) millisecond expiry to Unix seconds so the
	// returned value is comparable to time.Now().Unix() by callers.
	return value, normalizeExpiry(expiry, schemaVer)
}
