// Package clientip determines the real client address of a request behind
// trusted reverse proxies.
package clientip

import (
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
)

// From returns the client IP of r. X-Forwarded-For is only honored when
// the direct peer is a trusted proxy; then the rightmost address that is
// not itself a trusted proxy wins (left of it, anyone can write anything).
func From(r *http.Request, trusted []netip.Prefix) string {
	peer := parse(r.RemoteAddr)
	if !peer.IsValid() {
		return r.RemoteAddr
	}
	if !isTrusted(peer, trusted) {
		return peer.String()
	}
	var hops []string
	for _, h := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(h, ",")...)
	}
	for _, h := range slices.Backward(hops) {
		a := parse(strings.TrimSpace(h))
		if !a.IsValid() {
			break // garbage: stop, use the last good hop
		}
		peer = a
		if !isTrusted(a, trusted) {
			break
		}
	}
	return peer.String()
}

func isTrusted(a netip.Addr, trusted []netip.Prefix) bool {
	for _, p := range trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// parse accepts "ip", "ip:port" and "[ipv6]:port".
func parse(s string) netip.Addr {
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	a, err := netip.ParseAddr(strings.Trim(s, "[]"))
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap()
}

// ParsePrefixes parses CIDRs or plain addresses ("10.0.0.0/8", "127.0.0.1").
func ParsePrefixes(list []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, s := range list {
		s = strings.TrimSpace(s)
		if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(s)
		if err != nil {
			return nil, err
		}
		out = append(out, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
	}
	return out, nil
}
