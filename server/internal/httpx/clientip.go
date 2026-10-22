package httpx

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ClientIP returns the address of whoever sent r.
//
// Behind a reverse proxy (Caddy), RemoteAddr is the proxy itself, so every
// visitor would look like 127.0.0.1, and a limit "per address" would be one
// limit shared by everyone. The proxy adds the real address to
// X-Forwarded-For. That header is believed only when the request came from
// a trusted proxy, and it is read from the right: each proxy appends the
// address it got the request from, so the rightmost address that isn't one
// of our proxies is the client. Anything further left was written by the
// client and can be anything.
func ClientIP(r *http.Request, trusted []netip.Prefix) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || !contains(trusted, addr) {
		return host // a direct connection: RemoteAddr is the client
	}
	ips := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(ips) - 1; i >= 0; i-- {
		ip, err := netip.ParseAddr(strings.TrimSpace(ips[i]))
		if err != nil {
			break // garbage: stop rather than trust what's left of it
		}
		if !contains(trusted, ip) {
			return ip.Unmap().String()
		}
	}
	return host
}

func contains(prefixes []netip.Prefix, ip netip.Addr) bool {
	ip = ip.Unmap()
	for _, p := range prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// ParsePrefixes reads a comma-separated list of addresses or CIDR ranges,
// like "127.0.0.1, 10.0.0.0/8".
func ParsePrefixes(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if !strings.Contains(f, "/") {
			a, err := netip.ParseAddr(f)
			if err != nil {
				return nil, fmt.Errorf("%q is not an IP address or CIDR range", f)
			}
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
			continue
		}
		p, err := netip.ParsePrefix(f)
		if err != nil {
			return nil, fmt.Errorf("%q is not an IP address or CIDR range", f)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}
