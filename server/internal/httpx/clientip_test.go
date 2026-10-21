package httpx

import (
	"net/http/httptest"
	"testing"
)

func TestClientIP(t *testing.T) {
	loopback, _ := ParsePrefixes("127.0.0.1, ::1")
	for _, tc := range []struct {
		name, remote string
		xff          []string
		want         string
	}{
		{"direct, no header", "203.0.113.5:4000", nil, "203.0.113.5"},
		// A client connecting directly can't pick its address by sending
		// the header: it's only read from trusted proxies.
		{"direct, forged header", "203.0.113.5:4000", []string{"1.2.3.4"}, "203.0.113.5"},
		{"via Caddy", "127.0.0.1:5000", []string{"198.51.100.7"}, "198.51.100.7"},
		// The client sent its own X-Forwarded-For; Caddy appended the real
		// address. The rightmost untrusted one wins, not the forged one.
		{"via Caddy, client forged the left side", "127.0.0.1:5000", []string{"1.2.3.4, 198.51.100.7"}, "198.51.100.7"},
		{"via Caddy, two header lines", "127.0.0.1:5000", []string{"1.2.3.4", "198.51.100.7"}, "198.51.100.7"},
		{"IPv6 proxy and client", "[::1]:5000", []string{"2001:db8::9"}, "2001:db8::9"},
		{"proxy without a header", "127.0.0.1:5000", nil, "127.0.0.1"},
		{"garbage in the header", "127.0.0.1:5000", []string{"not-an-ip"}, "127.0.0.1"},
		{"IPv4-mapped IPv6", "127.0.0.1:5000", []string{"::ffff:198.51.100.7"}, "198.51.100.7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/", nil)
			r.RemoteAddr = tc.remote
			for _, v := range tc.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := ClientIP(r, loopback); got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestParsePrefixes(t *testing.T) {
	p, err := ParsePrefixes(" 127.0.0.1 , 10.0.0.0/8,::1,")
	if err != nil || len(p) != 3 || p[1].String() != "10.0.0.0/8" || p[0].Bits() != 32 {
		t.Fatalf("got %v, %v", p, err)
	}
	if _, err := ParsePrefixes("10.0.0.0/33"); err == nil {
		t.Error("bad prefix accepted")
	}
}
