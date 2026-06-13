package browser

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/sha1"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	_ "modernc.org/sqlite"
)

// Chrome (and other Chromium browsers) store cookie values AES-128-CBC encrypted
// on macOS, keyed off a password held in the login Keychain ("Chrome Safe
// Storage"). This file mirrors the Firefox plaintext reader but adds the
// decryption Chromium needs. Reading the Keychain password triggers a one-time
// GUI authorization prompt the first time cass asks for it.

// chromiumKeychainService maps a browser name to its Keychain service + account
// (the "<Browser> Safe Storage" generic-password entry).
func chromiumKeychainService(browser string) (service, account string) {
	switch strings.ToLower(browser) {
	case "brave":
		return "Brave Safe Storage", "Brave"
	case "edge":
		return "Microsoft Edge Safe Storage", "Microsoft Edge"
	case "chromium":
		return "Chromium Safe Storage", "Chromium"
	default: // chrome
		return "Chrome Safe Storage", "Chrome"
	}
}

// chromiumUserDataDir returns the macOS user-data root for a Chromium browser.
func chromiumUserDataDir(browser string) string {
	home, _ := os.UserHomeDir()
	base := filepath.Join(home, "Library", "Application Support")
	switch strings.ToLower(browser) {
	case "brave":
		return filepath.Join(base, "BraveSoftware", "Brave-Browser")
	case "edge":
		return filepath.Join(base, "Microsoft Edge")
	case "chromium":
		return filepath.Join(base, "Chromium")
	default:
		return filepath.Join(base, "Google", "Chrome")
	}
}

// chromiumSafeStorageKey derives the AES key Chromium uses for cookie values:
// PBKDF2-HMAC-SHA1(keychain password, salt="saltysalt", 1003 iters, 16 bytes).
// Reading the Keychain password may pop a one-time authorization dialog.
func chromiumSafeStorageKey(browser string) ([]byte, error) {
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("Chromium cookie decryption only implemented on macOS")
	}
	service, account := chromiumKeychainService(browser)
	out, err := exec.Command("security", "find-generic-password", "-w", "-s", service, "-a", account).Output()
	if err != nil {
		return nil, fmt.Errorf("read %q from Keychain (allow the prompt, or the entry is missing): %w", service, err)
	}
	pw := strings.TrimRight(string(out), "\n")
	return pbkdf2.Key(sha1.New, pw, []byte("saltysalt"), 1003, 16)
}

// FindChromiumCookiesDB returns the Cookies sqlite path for the given browser +
// profile (profile "" → "Default"), or "" if not found.
func FindChromiumCookiesDB(browser, profile string) string {
	if profile == "" {
		profile = "Default"
	}
	root := chromiumUserDataDir(browser)
	// Chromium keeps Cookies under <root>/<profile>/Cookies (newer builds nest
	// it under .../Network/Cookies).
	for _, p := range []string{
		filepath.Join(root, profile, "Network", "Cookies"),
		filepath.Join(root, profile, "Cookies"),
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// decryptChromiumCookie decrypts a single encrypted_value blob. v10/v11 blobs
// are AES-128-CBC with a 16-space IV; the rest is the ciphertext. Newer Chrome
// (M127+) prepends a 32-byte SHA-256 domain hash to the plaintext, which we
// strip when the leading 32 bytes aren't printable.
func decryptChromiumCookie(enc, key []byte) (string, error) {
	if len(enc) < 3 {
		return "", fmt.Errorf("cookie value too short")
	}
	prefix := string(enc[:3])
	if prefix != "v10" && prefix != "v11" {
		return string(enc), nil // not encrypted (rare on macOS)
	}
	ct := enc[3:]
	if len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		return "", fmt.Errorf("ciphertext not block-aligned")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	iv := bytes.Repeat([]byte{' '}, aes.BlockSize)
	pt := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(pt, ct)
	pt = pkcs7Unpad(pt)
	// Strip the optional 32-byte domain-hash prefix Chrome M127+ prepends.
	if len(pt) > 32 && !isPrintableASCII(pt[:32]) {
		pt = pt[32:]
	}
	return string(pt), nil
}

func pkcs7Unpad(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	n := int(b[len(b)-1])
	if n <= 0 || n > len(b) {
		return b
	}
	return b[:len(b)-n]
}

func isPrintableASCII(b []byte) bool {
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}

// ReadChromiumCookie returns (value, expiryUnixSeconds) for the first cookie
// matching hostLike (SQL LIKE) + name in the given browser/profile, decrypting
// the value. Returns "", 0 if not found or decryption fails.
func ReadChromiumCookie(browser, profile, hostLike, name string) (string, int64) {
	src := FindChromiumCookiesDB(browser, profile)
	if src == "" {
		return "", 0
	}
	key, err := chromiumSafeStorageKey(browser)
	if err != nil {
		return "", 0
	}
	// Copy the DB — Chrome holds a WAL lock while running.
	tmp, err := os.CreateTemp("", "cass-chrome-*.sqlite")
	if err != nil {
		return "", 0
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	sf, err := os.Open(src)
	if err != nil {
		tmp.Close()
		return "", 0
	}
	if _, err := io.Copy(tmp, sf); err != nil {
		sf.Close()
		tmp.Close()
		return "", 0
	}
	sf.Close()
	tmp.Close()

	db, err := sql.Open("sqlite", tmpPath+"?mode=ro")
	if err != nil {
		return "", 0
	}
	defer db.Close()

	var (
		enc        []byte
		expiresUTC int64
	)
	row := db.QueryRow(
		"SELECT encrypted_value, expires_utc FROM cookies WHERE host_key LIKE ? AND name = ? LIMIT 1",
		hostLike, name)
	if err := row.Scan(&enc, &expiresUTC); err != nil {
		return "", 0
	}
	val, err := decryptChromiumCookie(enc, key)
	if err != nil {
		return "", 0
	}
	// Chrome expires_utc is microseconds since 1601-01-01; convert to Unix sec.
	var expiry int64
	if expiresUTC > 0 {
		expiry = expiresUTC/1_000_000 - 11644473600
	}
	return val, expiry
}
