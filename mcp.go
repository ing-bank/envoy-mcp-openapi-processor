package envoy_mcp_openapi_processor

import "slices"

const (
	v20250618     = "2025-06-18"
	v20251125     = "2025-11-25"
	latestVersion = v20251125
)

var supportedProtocolVersions = []string{
	v20250618,
	v20251125,
}

// Version negotiation adopted from modelcontextprotocol/go-sdk.
// If clientVersion is supported, use it. Otherwise, use the latest supported version.
func negotiateVersion(clientVersion string) string {
	if slices.Contains(supportedProtocolVersions, clientVersion) {
		return clientVersion
	}
	return latestVersion
}
