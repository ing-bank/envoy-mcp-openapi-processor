package envoy_mcp_openapi_processor

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMcpRequestHandler_JSONRPCErrorCodes(t *testing.T) {
	tests := []struct {
		name        string
		requestBody []byte
		wantCode    int64
		wantMessage *regexp.Regexp
	}{
		{
			name:        "parse error on invalid JSON",
			requestBody: []byte(`{invalid json`),
			wantCode:    jsonrpc.CodeParseError,
			wantMessage: regexp.MustCompile("^Parse error$"),
		},
		{
			name:        "parse error on empty body",
			requestBody: []byte(``),
			wantCode:    jsonrpc.CodeParseError,
			wantMessage: regexp.MustCompile("^Parse error$"),
		},
		{
			name:        "invalid request on response object",
			requestBody: []byte(`{"jsonrpc":"2.0","id":1,"result":"something"}`),
			wantCode:    jsonrpc.CodeInvalidRequest,
			wantMessage: regexp.MustCompile("^Invalid Request$"),
		},
		{
			name:        "method not found for unknown method",
			requestBody: []byte(`{"jsonrpc":"2.0","id":1,"method":"unknown","params":{}}`),
			wantCode:    jsonrpc.CodeMethodNotFound,
			wantMessage: regexp.MustCompile("^Method not found$"),
		},
		{
			name:        "invalid params when initialize params is a string",
			requestBody: []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":"bad"}`),
			wantCode:    jsonrpc.CodeInvalidParams,
			wantMessage: regexp.MustCompile(`^Invalid params: json: .*`),
		},
		{
			name:        "invalid params when params is a string",
			requestBody: []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"bad"}`),
			wantCode:    jsonrpc.CodeInvalidParams,
			wantMessage: regexp.MustCompile("^Invalid params: json: .*"),
		},
		{
			name:        "invalid params when params is an array",
			requestBody: []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":["a","b"]}`),
			wantCode:    jsonrpc.CodeInvalidParams,
			wantMessage: regexp.MustCompile("^Invalid params: json: .*"),
		},
		{
			name:        "invalid params for unknown tool name",
			requestBody: []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"unknown_tool_id"}}`),
			wantCode:    jsonrpc.CodeInvalidParams,
			wantMessage: regexp.MustCompile("^Unknown tool: unknown_tool_id$"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := newTestServer(t, "testdata/petstore.openapi.yaml")

			result := server.mcpRequestHandler(context.Background(), tt.requestBody)

			require.NotNil(t, result)
			require.NotNil(t, result.ProcRep)

			immediate := result.ProcRep.GetImmediateResponse()
			require.NotNil(t, immediate, "protocol error should produce an immediate response")

			assert.Equal(t, int32(200), int32(immediate.GetStatus().GetCode()),
				"protocol errors should use HTTP 200 with JSON-RPC error body")

			assertJSONRPCErrorBody(t, immediate.GetBody(), tt.wantCode)

			var decoded map[string]any
			require.NoError(t, json.Unmarshal(immediate.GetBody(), &decoded))
			errObj := decoded["error"].(map[string]any)
			actualMsg := errObj["message"].(string)
			if tt.wantMessage != nil {
				assert.Regexp(t, tt.wantMessage, actualMsg)
			}
		})
	}
}

func assertJSONRPCErrorBody(t *testing.T, body []byte, wantCode int64) {
	t.Helper()

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(body, &decoded), "error body should be valid JSON")

	errObj, ok := decoded["error"].(map[string]any)
	require.True(t, ok, "response should contain 'error' field, got: %v", decoded)

	assert.NotContains(t, decoded, "result",
		"error response must not contain a 'result' field")

	rawCode, ok := errObj["code"].(float64)
	require.True(t, ok, "'error.code' should be a number, got: %v", errObj["code"])
	assert.Equal(t, wantCode, int64(rawCode), "JSON-RPC error code")

	_, hasMsg := errObj["message"].(string)
	assert.True(t, hasMsg, "'error.message' should be a string")
}
