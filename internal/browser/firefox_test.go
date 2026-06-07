package browser

import (
	"testing"
	"time"
)

// TestNormalizeExpiry covers the FF142+ (schema >= 16) millisecond expiry
// regression: yt-dlp divides expiry by 1000 for schema >= 16, older schemas
// keep seconds, and session cookies (expiry == 0) are never rescaled.
func TestNormalizeExpiry(t *testing.T) {
	cases := []struct {
		name      string
		expiry    int64
		schemaVer int
		want      int64
	}{
		// Real OTZ cookie from the verified FF142 DB: ms -> s is exact /1000.
		{"schema17_ms_to_s", 1782584677000, 17, 1782584677},
		{"schema16_ms_to_s", 1782584677000, 16, 1782584677},
		// Pre-FF142 schema stores seconds; leave untouched.
		{"schema15_seconds_unchanged", 1782584677, 15, 1782584677},
		{"schema0_seconds_unchanged", 1782584677, 0, 1782584677},
		// Session cookies (expiry == 0) must never be rescaled.
		{"session_cookie_schema17", 0, 17, 0},
		{"session_cookie_schema0", 0, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := normalizeExpiry(c.expiry, c.schemaVer); got != c.want {
				t.Fatalf("normalizeExpiry(%d, %d) = %d, want %d", c.expiry, c.schemaVer, got, c.want)
			}
		})
	}
}

// TestExpiryNowThreshold ensures the SQL WHERE threshold matches the unit of the
// raw expiry column: milliseconds for FF142+ (schema >= 16), seconds otherwise.
// Without this, the "expiry > now" filter compared ms expiry against seconds and
// never filtered, leaking already-expired cookies.
func TestExpiryNowThreshold(t *testing.T) {
	nowSec := time.Now().Unix()

	secThreshold := expiryNowThreshold(0)
	if secThreshold < nowSec-2 || secThreshold > nowSec+2 {
		t.Fatalf("expiryNowThreshold(0) = %d, want ~%d (seconds)", secThreshold, nowSec)
	}

	msThreshold := expiryNowThreshold(17)
	wantMs := nowSec * 1000
	if msThreshold < wantMs-2000 || msThreshold > wantMs+2000 {
		t.Fatalf("expiryNowThreshold(17) = %d, want ~%d (milliseconds)", msThreshold, wantMs)
	}

	// The ms threshold must be ~1000x the seconds threshold so an expired ms
	// expiry (just below now*1000) is correctly excluded.
	if msThreshold < secThreshold*900 {
		t.Fatalf("ms threshold %d not ~1000x seconds threshold %d", msThreshold, secThreshold)
	}
}
