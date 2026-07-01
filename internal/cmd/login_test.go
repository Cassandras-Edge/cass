package cmd

import "testing"

func TestParseCallbackInput(t *testing.T) {
	// A base64 secret containing '+' must survive round-trip: in a real
	// redirect URL it is percent-encoded (%2B), so ParseQuery restores the '+'.
	const key = "abc+/def=="
	const secret = "s3cr+t=="

	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{
			name: "full localhost url",
			in:   "http://localhost:52341/callback?key=" + urlEnc(key) + "&email=a%40b.com&cf_client_id=cid&cf_client_secret=" + urlEnc(secret),
		},
		{
			name: "bare query string",
			in:   "key=" + urlEnc(key) + "&email=a%40b.com",
		},
		{
			name: "query with leading question mark",
			in:   "?key=k&email=a%40b.com",
		},
		{
			name: "surrounding whitespace and newline",
			in:   "  http://localhost:9/callback?key=k&email=a%40b.com \n",
		},
		{name: "empty", in: "", wantErr: true},
		{name: "whitespace only", in: "   \n", wantErr: true},
		{name: "portal url without key/email", in: "https://portal.example.com/api/cli/login?callback=http%3A%2F%2Flocalhost%3A9%2Fcallback&device=box", wantErr: true},
		{name: "missing email", in: "key=k", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vals, err := parseCallbackInput(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got vals=%v", vals)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if vals.Get("email") != "a@b.com" {
				t.Errorf("email = %q, want a@b.com", vals.Get("email"))
			}
		})
	}

	// The '+' in a percent-encoded key must not be corrupted into a space.
	vals, err := parseCallbackInput("key=" + urlEnc(key) + "&email=a%40b.com&cf_client_secret=" + urlEnc(secret))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vals.Get("key") != key {
		t.Errorf("key = %q, want %q (percent-encoded '+' corrupted?)", vals.Get("key"), key)
	}
	if vals.Get("cf_client_secret") != secret {
		t.Errorf("secret = %q, want %q", vals.Get("cf_client_secret"), secret)
	}
}

// urlEnc percent-encodes a value the way a browser puts it in the redirect URL,
// so the test exercises the same decoding path as a real paste.
func urlEnc(s string) string {
	out := ""
	for _, b := range []byte(s) {
		if (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '-' || b == '_' || b == '.' || b == '~' {
			out += string(b)
			continue
		}
		const hex = "0123456789ABCDEF"
		out += "%" + string(hex[b>>4]) + string(hex[b&0xf])
	}
	return out
}
