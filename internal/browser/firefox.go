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
	"time"

	_ "modernc.org/sqlite"
)

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
// running and holding a WAL lock) and opens it read-only. Caller must close
// the returned DB and remove the temp file.
func openFirefoxCopy() (*sql.DB, string, error) {
	src := FindFirefoxCookiesDB()
	if src == "" {
		return nil, "", fmt.Errorf("Firefox cookies.sqlite not found")
	}
	tmp, err := os.CreateTemp("", "cass-fox-*.sqlite")
	if err != nil {
		return nil, "", err
	}
	tmpPath := tmp.Name()
	sf, err := os.Open(src)
	if err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return nil, "", err
	}
	if _, err := io.Copy(tmp, sf); err != nil {
		sf.Close()
		tmp.Close()
		os.Remove(tmpPath)
		return nil, "", err
	}
	sf.Close()
	tmp.Close()
	db, err := sql.Open("sqlite", tmpPath+"?mode=ro")
	if err != nil {
		os.Remove(tmpPath)
		return nil, "", err
	}
	return db, tmpPath, nil
}

// HasFirefoxCookies returns true iff Firefox holds at least one non-expired
// cookie matching `domains`. When `requiredNames` is non-empty, ALL of those
// cookie names must be present (and non-expired) on the matching hosts.
// Fast: typical query is <5 ms after the SQLite copy.
func HasFirefoxCookies(domains []string, requiredNames []string) bool {
	db, tmp, err := openFirefoxCopy()
	if err != nil {
		return false
	}
	defer db.Close()
	defer os.Remove(tmp)

	now := time.Now().Unix()
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

// ReadFirefoxCookie returns (value, expiryUnix) for the first cookie matching
// hostLike (SQL LIKE pattern, e.g. "%perplexity.ai") + name, or empty/0 if
// not found. Used by `cookies refresh` to poll cf_clearance rotation.
func ReadFirefoxCookie(hostLike, name string) (string, int64) {
	db, tmp, err := openFirefoxCopy()
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
	return value, expiry
}
