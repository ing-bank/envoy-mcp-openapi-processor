package envoy_mcp_openapi_processor

import (
	"net"
	"net/url"
	"strings"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
)

// defaultAllowedHosts is the DNS-rebinding-safe default policy: only loopback
// hostnames are accepted in the Host/:authority and Origin headers.
var defaultAllowedHosts = []string{"localhost", "127.0.0.1", "::1"}

type hostAllowlist struct {
	// allowAny is true when "*" was configured: every host matches.
	allowAny bool
	// exact holds normalized (lowercased, port stripped) hostnames matched
	// verbatim.
	exact map[string]struct{}
	// suffixes holds the ".example.org" forms derived from "*.example.org"
	// entries (leading dot included). A host matches when it ends with one of
	// these, so any subdomain at any depth is accepted but the apex is not.
	suffixes []string
}

// newHostAllowlist builds an allowlist from the configured hostnames. An empty
// list means the localhost-only default. Three entry forms are supported:
//   - "*" allows any host;
//   - "*.example.org" allows any subdomain (at any depth) but not the apex;
//   - a plain hostname is matched exactly.
//
// Plain entries are normalized like header values (lowercased, brackets and
// port stripped), so "[::1]", "::1" and "mcp.example.com:8080" all work.
func newHostAllowlist(allowed []string) *hostAllowlist {
	if len(allowed) == 0 {
		allowed = defaultAllowedHosts
	}
	a := &hostAllowlist{exact: make(map[string]struct{}, len(allowed))}
	for _, h := range allowed {
		switch {
		case h == "*":
			a.allowAny = true
		case strings.HasPrefix(h, "*."):
			// Store ".example.org" (leading dot kept) so matching is a plain
			// suffix test; wildcard entries carry no port to strip.
			a.suffixes = append(a.suffixes, strings.ToLower(h[1:]))
		default:
			a.exact[normalizeHostname(h)] = struct{}{}
		}
	}
	return a
}

// normalizeHostname extracts the lowercased hostname from a host[:port] value.
// Malformed values normalize to something that won't match any entry, so
// validation fails closed.
func normalizeHostname(v string) string {
	v = strings.TrimSpace(v)
	if host, _, err := net.SplitHostPort(v); err == nil {
		v = host // SplitHostPort also unbrackets "[::1]:8080" -> "::1"
	} else if strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]") {
		v = v[1 : len(v)-1] // portless bracketed IPv6: "[::1]" -> "::1"
	}
	return strings.ToLower(v)
}

func (a *hostAllowlist) allows(hostname string) bool {
	if a.allowAny {
		return true
	}
	if _, ok := a.exact[hostname]; ok {
		return true
	}
	for _, suffix := range a.suffixes {
		if strings.HasSuffix(hostname, suffix) {
			return true
		}
	}
	return false
}

// checkHeaders reports whether the request's Host/:authority and Origin
// headers are allowed; reason describes the first failed check. A missing
// Host fails closed; a missing Origin is allowed (non-browser clients do not
// send one), but a present Origin must be a URL whose hostname is allowed,
// which rejects opaque origins such as "null".
func (a *hostAllowlist) checkHeaders(headers *corev3.HeaderMap) (bool, string) {
	host, ok := findHeader(headers, ":authority")
	if !ok {
		host, ok = findHeader(headers, "host")
	}
	if !ok || host == "" {
		return false, "missing host header"
	}
	if !a.allows(normalizeHostname(host)) {
		return false, "host not allowed: " + host
	}

	if origin, ok := findHeader(headers, "origin"); ok {
		u, err := url.Parse(origin)
		if err != nil || u.Hostname() == "" || !a.allows(strings.ToLower(u.Hostname())) {
			return false, "origin not allowed: " + origin
		}
	}

	return true, ""
}
