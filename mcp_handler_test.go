package envoy_mcp_openapi_processor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDescribeEmptyUpstreamResponse pins the text synthesized for a tool call
// whose upstream returned no body, across the status-code branches (unknown
// status, a status with a known reason phrase, and one without). The
// state-machine wiring that reaches this function is covered by
// TestProcess_ResponseHeadersEOS.
func TestDescribeEmptyUpstreamResponse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		statusCode int
		want       string
	}{
		{name: "unknown status", statusCode: 0, want: "API returned an unknown status with no response body."},
		{name: "known reason phrase", statusCode: 204, want: "API returned HTTP 204 (No Content) with no response body."},
		{name: "error status", statusCode: 500, want: "API returned HTTP 500 (Internal Server Error) with no response body."},
		{name: "no reason phrase", statusCode: 599, want: "API returned HTTP 599 (Unknown Status) with no response body."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, describeEmptyUpstreamResponse(tt.statusCode))
		})
	}
}

// TestHTTPStatusToErrorInfo pins the mapping from upstream HTTP status to the
// tool-error verdict: 2xx/3xx are successes (nil), while a missing status (0)
// and 4xx/5xx are tool errors carrying a reason.
func TestHTTPStatusToErrorInfo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		statusCode int
		wantError  bool
	}{
		{name: "ok", statusCode: 200, wantError: false},
		{name: "no content", statusCode: 204, wantError: false},
		{name: "redirect", statusCode: 302, wantError: false},
		{name: "unknown status", statusCode: 0, wantError: true},
		{name: "not found", statusCode: 404, wantError: true},
		{name: "server error", statusCode: 500, wantError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			errInfo := httpStatusToErrorInfo(tt.statusCode)
			if !tt.wantError {
				assert.Nil(t, errInfo)
				return
			}
			require.NotNil(t, errInfo)
			assert.NotEmpty(t, errInfo.traceMessage)
		})
	}
}
