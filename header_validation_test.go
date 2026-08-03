package envoy_mcp_openapi_processor

import (
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	"github.com/stretchr/testify/assert"
)

func TestCaptureMCPRequestHeaders(t *testing.T) {
	t.Parallel()

	headers := &corev3.HeaderMap{Headers: []*corev3.HeaderValue{
		{Key: ":authority", RawValue: []byte("localhost")},
		{Key: "mcp-protocol-version", RawValue: []byte("2026-07-28")},
		{Key: "mcp-method", RawValue: []byte("tools/call")},
		{Key: "mcp-name", RawValue: []byte("getPetById")},
		{Key: "mcp-param-api-key", RawValue: []byte("secret")},
		{Key: "mcp-param-x-trace", RawValue: []byte("abc")},
		{Key: "content-type", RawValue: []byte("application/json")},
	}}

	captured := captureMCPRequestHeaders(headers)
	assert.Equal(t, "2026-07-28", captured.protocolVersion)
	assert.Equal(t, "tools/call", captured.method)
	assert.Equal(t, "getPetById", captured.name)
	assert.ElementsMatch(t, []string{"mcp-param-api-key", "mcp-param-x-trace"}, captured.paramHeaderNames)
}

func TestCaptureMCPRequestHeaders_CaseInsensitiveNames(t *testing.T) {
	t.Parallel()

	headers := &corev3.HeaderMap{Headers: []*corev3.HeaderValue{
		{Key: "MCP-Protocol-Version", RawValue: []byte("2026-07-28")},
		{Key: "Mcp-Method", RawValue: []byte("tools/call")},
		{Key: "MCP-NAME", RawValue: []byte("getPetById")},
		{Key: "Mcp-Param-Region", RawValue: []byte("us-west1")},
	}}

	captured := captureMCPRequestHeaders(headers)
	assert.Equal(t, "2026-07-28", captured.protocolVersion)
	assert.Equal(t, "tools/call", captured.method)
	assert.Equal(t, "getPetById", captured.name)
	assert.Equal(t, []string{"mcp-param-region"}, captured.paramHeaderNames)
}

func TestCaptureMCPRequestHeaders_TrimsSurroundingWhitespace(t *testing.T) {
	t.Parallel()

	headers := &corev3.HeaderMap{Headers: []*corev3.HeaderValue{
		{Key: "mcp-name", RawValue: []byte("  getPetById ")},
	}}

	assert.Equal(t, "getPetById", captureMCPRequestHeaders(headers).name)
}

// First wins even when it is empty.
func TestCaptureMCPRequestHeaders_RepeatedHeaderKeepsFirst(t *testing.T) {
	t.Parallel()

	headers := &corev3.HeaderMap{Headers: []*corev3.HeaderValue{
		{Key: "mcp-method", RawValue: []byte("tools/list")},
		{Key: "Mcp-Method", RawValue: []byte("tools/call")},
		{Key: "mcp-name", RawValue: []byte("")},
		{Key: "mcp-name", RawValue: []byte("getPetById")},
	}}

	captured := captureMCPRequestHeaders(headers)
	assert.Equal(t, "tools/list", captured.method)
	assert.Empty(t, captured.name)
}

func TestCaptureMCPRequestHeaders_Empty(t *testing.T) {
	t.Parallel()

	captured := captureMCPRequestHeaders(&corev3.HeaderMap{})
	assert.Empty(t, captured.protocolVersion)
	assert.Empty(t, captured.method)
	assert.Empty(t, captured.name)
	assert.Empty(t, captured.paramHeaderNames)
}

func TestDecodeHeaderValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		input  string
		want   string
		wantOK bool
	}{
		{name: "plain value passes through", input: "getPetById", want: "getPetById", wantOK: true},
		{name: "empty value passes through", input: "", want: "", wantOK: true},
		{name: "base64 sentinel decodes", input: "=?base64?Z2V0UGV0QnlJZA==?=", want: "getPetById", wantOK: true},
		{name: "base64 sentinel with non-ascii payload", input: "=?base64?w6l0w6k=?=", want: "été", wantOK: true},
		{name: "invalid base64 payload fails", input: "=?base64?!!!?=", wantOK: false},
		{name: "prefix without suffix passes through verbatim", input: "=?base64?abc", want: "=?base64?abc", wantOK: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := decodeHeaderValue(tt.input)
			assert.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}
