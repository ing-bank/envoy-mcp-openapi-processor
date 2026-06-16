package envoy_mcp_openapi_processor

import (
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	"github.com/stretchr/testify/assert"
)

func headerMap(pairs ...string) *corev3.HeaderMap {
	headers := make([]*corev3.HeaderValue, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		headers = append(headers, &corev3.HeaderValue{Key: pairs[i], RawValue: []byte(pairs[i+1])})
	}
	return &corev3.HeaderMap{Headers: headers}
}

func TestHostAllowlist_CheckHeaders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		allowed []string
		headers *corev3.HeaderMap
		wantOK  bool
	}{
		// Default policy: Host variations.
		{name: "localhost", headers: headerMap(":authority", "localhost"), wantOK: true},
		{name: "localhost_uppercase", headers: headerMap(":authority", "LOCALHOST"), wantOK: true},
		{name: "localhost_with_port", headers: headerMap(":authority", "localhost:8080"), wantOK: true},
		{name: "loopback_ipv4", headers: headerMap(":authority", "127.0.0.1"), wantOK: true},
		{name: "loopback_ipv4_with_port", headers: headerMap(":authority", "127.0.0.1:9"), wantOK: true},
		{name: "loopback_ipv6_bare", headers: headerMap(":authority", "::1"), wantOK: true},
		{name: "loopback_ipv6_bracketed", headers: headerMap(":authority", "[::1]"), wantOK: true},
		{name: "loopback_ipv6_bracketed_with_port", headers: headerMap(":authority", "[::1]:8080"), wantOK: true},
		{name: "host_header_fallback", headers: headerMap("host", "localhost"), wantOK: true},

		// Default policy: Host rejections (fail closed).
		{name: "missing_host", headers: headerMap(":method", "POST"), wantOK: false},
		{name: "empty_host", headers: headerMap(":authority", ""), wantOK: false},
		{name: "external_host", headers: headerMap(":authority", "evil.com"), wantOK: false},
		{name: "external_host_with_port", headers: headerMap(":authority", "evil.com:80"), wantOK: false},
		{name: "other_loopback_ip", headers: headerMap(":authority", "127.0.0.2"), wantOK: false},
		{name: "localhost_subdomain_of_attacker", headers: headerMap(":authority", "localhost.evil.com"), wantOK: false},
		{name: "subdomain_of_localhost", headers: headerMap(":authority", "sub.localhost"), wantOK: false},
		{name: "malformed_host", headers: headerMap(":authority", "localhost:80:90"), wantOK: false},

		// Default policy: Origin variations (Host valid).
		{name: "origin_absent", headers: headerMap(":authority", "localhost"), wantOK: true},
		{name: "origin_localhost_with_port", headers: headerMap(":authority", "localhost", "origin", "http://localhost:6274"), wantOK: true},
		{name: "origin_https_localhost", headers: headerMap(":authority", "localhost", "origin", "https://localhost"), wantOK: true},
		{name: "origin_uppercase", headers: headerMap(":authority", "localhost", "origin", "HTTP://LOCALHOST:6274"), wantOK: true},
		{name: "origin_ipv6", headers: headerMap(":authority", "localhost", "origin", "http://[::1]:6274"), wantOK: true},
		{name: "origin_external", headers: headerMap(":authority", "localhost", "origin", "http://evil.com"), wantOK: false},
		{name: "origin_null", headers: headerMap(":authority", "localhost", "origin", "null"), wantOK: false},
		{name: "origin_garbage", headers: headerMap(":authority", "localhost", "origin", "%%%"), wantOK: false},
		{name: "origin_without_scheme", headers: headerMap(":authority", "localhost", "origin", "localhost:6274"), wantOK: false},
		{name: "bad_host_good_origin", headers: headerMap(":authority", "evil.com", "origin", "http://localhost"), wantOK: false},

		// Custom allowlist replaces the default.
		{name: "custom_host_with_port", allowed: []string{"MCP.Example.COM"}, headers: headerMap(":authority", "mcp.example.com:443"), wantOK: true},
		{name: "custom_origin", allowed: []string{"mcp.example.com"}, headers: headerMap(":authority", "mcp.example.com", "origin", "https://mcp.example.com"), wantOK: true},
		{name: "custom_rejects_localhost", allowed: []string{"mcp.example.com"}, headers: headerMap(":authority", "localhost"), wantOK: false},
		{name: "custom_bracketed_ipv6_entry", allowed: []string{"[::1]"}, headers: headerMap(":authority", "[::1]:8080"), wantOK: true},
		{name: "custom_entry_port_ignored", allowed: []string{"mcp.example.com:8080"}, headers: headerMap(":authority", "mcp.example.com:9090"), wantOK: true},

		// Wildcard "*": every host (and origin) is allowed.
		{name: "any_external_host", allowed: []string{"*"}, headers: headerMap(":authority", "evil.com"), wantOK: true},
		{name: "any_ip_host", allowed: []string{"*"}, headers: headerMap(":authority", "1.2.3.4:80"), wantOK: true},
		{name: "any_external_origin", allowed: []string{"*"}, headers: headerMap(":authority", "evil.com", "origin", "http://attacker.test"), wantOK: true},

		// Subdomain wildcard "*.example.org": any subdomain at any depth, not the apex.
		{name: "wildcard_subdomain", allowed: []string{"*.example.org"}, headers: headerMap(":authority", "foo.example.org"), wantOK: true},
		{name: "wildcard_nested_subdomain", allowed: []string{"*.example.org"}, headers: headerMap(":authority", "a.b.example.org"), wantOK: true},
		{name: "wildcard_subdomain_with_port", allowed: []string{"*.example.org"}, headers: headerMap(":authority", "foo.example.org:8443"), wantOK: true},
		{name: "wildcard_subdomain_uppercase", allowed: []string{"*.example.org"}, headers: headerMap(":authority", "FOO.EXAMPLE.ORG"), wantOK: true},
		{name: "wildcard_subdomain_origin", allowed: []string{"*.example.org"}, headers: headerMap(":authority", "foo.example.org", "origin", "https://bar.example.org"), wantOK: true},
		{name: "wildcard_rejects_apex", allowed: []string{"*.example.org"}, headers: headerMap(":authority", "example.org"), wantOK: false},
		{name: "wildcard_rejects_lookalike", allowed: []string{"*.example.org"}, headers: headerMap(":authority", "notexample.org"), wantOK: false},
		{name: "wildcard_rejects_suffix_attacker", allowed: []string{"*.example.org"}, headers: headerMap(":authority", "example.org.evil.test"), wantOK: false},

		// Mixed allowlist: exact, wildcard and plain entries coexist.
		{name: "mixed_exact_match", allowed: []string{"host.example.org", "*.corp.example", "localhost"}, headers: headerMap(":authority", "host.example.org"), wantOK: true},
		{name: "mixed_wildcard_match", allowed: []string{"host.example.org", "*.corp.example", "localhost"}, headers: headerMap(":authority", "vpn.corp.example"), wantOK: true},
		{name: "mixed_plain_match", allowed: []string{"host.example.org", "*.corp.example", "localhost"}, headers: headerMap(":authority", "localhost:8080"), wantOK: true},
		{name: "mixed_rejects_other", allowed: []string{"host.example.org", "*.corp.example", "localhost"}, headers: headerMap(":authority", "other.example.org"), wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, reason := newHostAllowlist(tt.allowed).checkHeaders(tt.headers)
			assert.Equal(t, tt.wantOK, ok)
			if !tt.wantOK {
				assert.NotEmpty(t, reason)
			}
		})
	}
}
