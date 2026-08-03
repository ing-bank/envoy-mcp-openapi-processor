package envoy_mcp_openapi_processor

import (
	"context"
	"testing"

	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const modernMeta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}`

func TestModernRequests_HappyPaths(t *testing.T) {
	t.Parallel()

	t.Run("tools/list carries modern result fields", func(t *testing.T) {
		t.Parallel()
		server := newTestServer(t, "testdata/petstore.openapi.yaml")
		server.serverInfo = ServerInfo{Name: "test-server", Version: "1.2.3"}

		requestBody := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{` + modernMeta + `}}`)
		headers := mcpRequestHeaders{protocolVersion: v20260728, method: methodToolsList}

		response := server.mcpRequestHandler(context.Background(), requestBody, headers)
		immediate := requireImmediateResponse(t, response.Immediate)
		assert.Equal(t, typev3.StatusCode_OK, immediate.GetStatus().GetCode())

		_, result := decodeJSONRPCResult(t, immediate.GetBody())
		assert.Equal(t, "complete", result["resultType"], "resultType")
		assert.Equal(t, float64(cacheTTLMillis), result["ttlMs"], "ttlMs")
		assert.Equal(t, cacheScope, result["cacheScope"], "cacheScope")
		assertServerInfoMeta(t, result, "test-server", "1.2.3")

		tools, ok := result["tools"].([]any)
		require.True(t, ok, "result should contain 'tools'")
		require.NotEmpty(t, tools)
	})

	t.Run("initialize never negotiates a modern version", func(t *testing.T) {
		t.Parallel()
		server := newTestServer(t, "testdata/petstore.openapi.yaml")

		requestBody := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2026-07-28","capabilities":{},` + modernMeta + `}}`)

		response := server.mcpRequestHandler(context.Background(), requestBody, mcpRequestHeaders{})
		immediate := requireImmediateResponse(t, response.Immediate)

		_, result := decodeJSONRPCResult(t, immediate.GetBody())
		assert.Equal(t, latestLegacyVersion, result["protocolVersion"])
		assert.NotContains(t, result, "resultType", "handshake result stays legacy-shaped")
	})
}

func TestModernRequests_ErrorPaths(t *testing.T) {
	t.Parallel()

	toolsCallBody := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"getPetById","arguments":{"petId":1},` + modernMeta + `}}`

	tests := []struct {
		name           string
		requestBody    string
		headers        mcpRequestHeaders
		wantHTTPStatus typev3.StatusCode
		wantCode       int64
		wantMessage    string
	}{
		{
			name:           "missing MCP-Protocol-Version header",
			requestBody:    `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` + modernMeta + `}}`,
			headers:        mcpRequestHeaders{method: methodToolsList},
			wantHTTPStatus: typev3.StatusCode_BadRequest,
			wantCode:       mcp.CodeHeaderMismatch,
			wantMessage:    "missing required MCP-Protocol-Version header",
		},
		{
			name:           "modern header without body _meta",
			requestBody:    `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`,
			headers:        mcpRequestHeaders{protocolVersion: v20260728, method: methodToolsList},
			wantHTTPStatus: typev3.StatusCode_BadRequest,
			wantCode:       mcp.CodeHeaderMismatch,
		},
		{
			name:           "protocol version mismatch",
			requestBody:    `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` + modernMeta + `}}`,
			headers:        mcpRequestHeaders{protocolVersion: "2027-01-01", method: methodToolsList},
			wantHTTPStatus: typev3.StatusCode_BadRequest,
			wantCode:       mcp.CodeHeaderMismatch,
		},
		{
			name:           "missing Mcp-Method header",
			requestBody:    `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` + modernMeta + `}}`,
			headers:        mcpRequestHeaders{protocolVersion: v20260728},
			wantHTTPStatus: typev3.StatusCode_BadRequest,
			wantCode:       mcp.CodeHeaderMismatch,
			wantMessage:    "missing required Mcp-Method header",
		},
		{
			name:           "Mcp-Method mismatch",
			requestBody:    `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` + modernMeta + `}}`,
			headers:        mcpRequestHeaders{protocolVersion: v20260728, method: methodToolsCall},
			wantHTTPStatus: typev3.StatusCode_BadRequest,
			wantCode:       mcp.CodeHeaderMismatch,
		},
		{
			name:           "missing Mcp-Name header for tools/call",
			requestBody:    toolsCallBody,
			headers:        mcpRequestHeaders{protocolVersion: v20260728, method: methodToolsCall},
			wantHTTPStatus: typev3.StatusCode_BadRequest,
			wantCode:       mcp.CodeHeaderMismatch,
		},
		{
			name:           "Mcp-Name mismatch",
			requestBody:    toolsCallBody,
			headers:        mcpRequestHeaders{protocolVersion: v20260728, method: methodToolsCall, name: "otherTool"},
			wantHTTPStatus: typev3.StatusCode_BadRequest,
			wantCode:       mcp.CodeHeaderMismatch,
		},
		{
			name:        "Mcp-Name mismatch reports the decoded value",
			requestBody: toolsCallBody,
			// "otherTool" wrapped in the =?base64?...?= sentinel
			headers:        mcpRequestHeaders{protocolVersion: v20260728, method: methodToolsCall, name: "=?base64?b3RoZXJUb29s?="},
			wantHTTPStatus: typev3.StatusCode_BadRequest,
			wantCode:       mcp.CodeHeaderMismatch,
			wantMessage:    "header mismatch: Mcp-Name header value 'otherTool' does not match body value 'getPetById'",
		},
		{
			name:           "Mcp-Name invalid Base64 sentinel",
			requestBody:    toolsCallBody,
			headers:        mcpRequestHeaders{protocolVersion: v20260728, method: methodToolsCall, name: "=?base64?!!!?="},
			wantHTTPStatus: typev3.StatusCode_BadRequest,
			wantCode:       mcp.CodeHeaderMismatch,
		},
		{
			name:           "unknown method gets 404",
			requestBody:    `{"jsonrpc":"2.0","id":1,"method":"resources/list","params":{` + modernMeta + `}}`,
			headers:        mcpRequestHeaders{protocolVersion: v20260728, method: "resources/list"},
			wantHTTPStatus: typev3.StatusCode_NotFound,
			wantCode:       -32601,
			wantMessage:    "Method not found",
		},
		{
			name:           "unknown tool gets 400",
			requestBody:    `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"noSuchTool","arguments":{},` + modernMeta + `}}`,
			headers:        mcpRequestHeaders{protocolVersion: v20260728, method: methodToolsCall, name: "noSuchTool"},
			wantHTTPStatus: typev3.StatusCode_BadRequest,
			wantCode:       jsonrpc.CodeInvalidParams,
			wantMessage:    "Unknown tool: noSuchTool",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := newTestServer(t, "testdata/petstore.openapi.yaml")

			response := server.mcpRequestHandler(context.Background(), []byte(tt.requestBody), tt.headers)
			require.NotNil(t, response)
			immediate := requireImmediateResponse(t, response.Immediate)
			assert.Equal(t, tt.wantHTTPStatus, immediate.GetStatus().GetCode(), "HTTP status")

			errObj := assertJSONRPCErrorBody(t, immediate.GetBody(), tt.wantCode)
			if tt.wantMessage != "" {
				assert.Equal(t, tt.wantMessage, errObj["message"])
			}
		})
	}
}

// The legacy era imposes no mirrored-header contract, so the declaration is
// ignored, headers included.
func TestModernRequests_PreCutoverMetaVersionIsIgnored(t *testing.T) {
	t.Parallel()

	legacyMeta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2025-11-25"}`
	legacyHeaders := mcpRequestHeaders{protocolVersion: v20251125}

	t.Run("tools/list is served unshaped", func(t *testing.T) {
		t.Parallel()
		server := newTestServer(t, "testdata/petstore.openapi.yaml")

		response := server.mcpRequestHandler(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{`+legacyMeta+`}}`), legacyHeaders)
		immediate := requireImmediateResponse(t, response.Immediate)
		assert.Equal(t, typev3.StatusCode_OK, immediate.GetStatus().GetCode())

		_, result := decodeJSONRPCResult(t, immediate.GetBody())
		assert.NotContains(t, result, "resultType", "an ignored declaration must not shape the result")
		assert.NotContains(t, result, "_meta")
		assert.NotContains(t, result, "ttlMs", "legacy result should not carry cache hints")
		assert.NotContains(t, result, "cacheScope", "legacy result should not carry cache hints")
	})

	t.Run("tools/call reroutes on the legacy era", func(t *testing.T) {
		t.Parallel()
		server := newTestServer(t, "testdata/petstore.openapi.yaml")

		response := server.mcpRequestHandler(context.Background(), []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"getPetById","arguments":{"petId":1},`+legacyMeta+`}}`), legacyHeaders)
		require.NotNil(t, response.Reroute)
		assert.IsType(t, legacyEra{}, response.Era)
	})
}

func TestModernRequests_UnsupportedVersion(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, "testdata/petstore.openapi.yaml")
	requestBody := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2099-01-01"}}}`)
	headers := mcpRequestHeaders{protocolVersion: "2099-01-01", method: methodToolsList}

	response := server.mcpRequestHandler(context.Background(), requestBody, headers)
	immediate := requireImmediateResponse(t, response.Immediate)
	assert.Equal(t, typev3.StatusCode_BadRequest, immediate.GetStatus().GetCode(), "HTTP status")

	errObj := assertJSONRPCErrorBody(t, immediate.GetBody(), mcp.CodeUnsupportedProtocolVersion)
	assert.Equal(t, "Unsupported protocol version: 2099-01-01", errObj["message"])
	data, ok := errObj["data"].(map[string]any)
	require.True(t, ok, "error should carry structured data")
	assert.Equal(t, "2099-01-01", data["requested"])
	assert.Equal(t, []any{"2026-07-28", "2025-11-25", "2025-06-18"}, data["supported"])
}

func TestToolCallResponseShapingPerEra(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		era        protocolEra
		wantModern bool
	}{
		{
			name:       "modern response carries resultType and serverInfo",
			era:        modernEra{serverInfo: ServerInfo{Name: "test-server", Version: "1.2.3"}},
			wantModern: true,
		},
		{name: "legacy response stays unshaped", era: legacyEra{}, wantModern: false},
		{name: "an unset era defaults to legacy", era: nil, wantModern: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reqCtx := &requestContext{
				jsonrpcID:          mustMakeID("shape-1"),
				toolResponseConfig: &toolResponseConfig{},
				upstreamStatusCode: 200,
				era:                tt.era,
			}
			mutation := mcpResponseHandler(context.Background(), []byte(`{"status":"ok"}`), reqCtx)
			require.NotNil(t, mutation.headers)

			_, result := decodeJSONRPCResult(t, mutation.body)
			if tt.wantModern {
				assert.Equal(t, "complete", result["resultType"])
				assertServerInfoMeta(t, result, "test-server", "1.2.3")
			} else {
				assert.NotContains(t, result, "resultType")
				assert.NotContains(t, result, "_meta")
			}
			content, ok := result["content"].([]any)
			require.True(t, ok, "result should contain 'content'")
			require.NotEmpty(t, content)
		})
	}
}

func TestModernToolCallStripsMCPHeadersOnReroute(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, "testdata/petstore.openapi.yaml")
	requestBody := []byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"getPetById","arguments":{"petId":1},` + modernMeta + `}}`)
	headers := mcpRequestHeaders{
		protocolVersion:  v20260728,
		method:           methodToolsCall,
		name:             "getPetById",
		paramHeaderNames: []string{"mcp-param-x-trace"},
	}

	response := server.mcpRequestHandler(context.Background(), requestBody, headers)
	require.NotNil(t, response.Reroute, "modern tools/call should reroute upstream")
	assert.IsType(t, modernEra{}, response.Era)

	removed := response.Reroute.headers.GetRequestHeaders().GetResponse().GetHeaderMutation().GetRemoveHeaders()
	assert.Contains(t, removed, headerMCPProtocolVersion)
	assert.Contains(t, removed, headerMCPMethod)
	assert.Contains(t, removed, headerMCPName)
	assert.Contains(t, removed, "mcp-param-x-trace")
}
