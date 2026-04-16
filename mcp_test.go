package envoy_mcp_openapi_processor

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNegotiateVersion(t *testing.T) {
	tests := []struct {
		name          string
		clientVersion string
		want          string
	}{
		{
			name:          "uses supported client version",
			clientVersion: v20250618,
			want:          v20250618,
		},
		{
			name:          "defaults to latest supported version",
			clientVersion: "2024-11-05",
			want:          latestVersion,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, negotiateVersion(test.clientVersion))
		})
	}
}
