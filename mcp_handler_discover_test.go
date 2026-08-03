package envoy_mcp_openapi_processor

import (
	"context"
	"testing"

	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServerDiscover(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, "testdata/petstore.openapi.yaml")
	server.serverInfo = ServerInfo{Name: "test-server", Version: "1.2.3", Instructions: "test instructions"}

	requestBody := []byte(`{"jsonrpc":"2.0","id":7,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`)
	headers := mcpRequestHeaders{protocolVersion: v20260728, method: methodDiscover}

	response := server.mcpRequestHandler(context.Background(), requestBody, headers)
	require.NotNil(t, response)
	immediate := requireImmediateResponse(t, response.Immediate)
	assert.Equal(t, typev3.StatusCode_OK, immediate.GetStatus().GetCode())

	envelope, result := decodeJSONRPCResult(t, immediate.GetBody())
	assert.Equal(t, float64(7), envelope["id"], "jsonrpc id")

	assert.Equal(t, "complete", result["resultType"], "resultType")
	assert.Equal(t, []any{"2026-07-28", "2025-11-25", "2025-06-18"}, result["supportedVersions"], "supportedVersions newest first")

	capabilities, ok := result["capabilities"].(map[string]any)
	require.True(t, ok, "result should contain 'capabilities'")
	_, hasTools := capabilities["tools"]
	assert.True(t, hasTools, "capabilities should advertise tools")

	assert.Equal(t, "test instructions", result["instructions"], "instructions")
	assert.Equal(t, float64(cacheTTLMillis), result["ttlMs"], "ttlMs")
	assert.Equal(t, cacheScope, result["cacheScope"], "cacheScope")
	assertServerInfoMeta(t, result, "test-server", "1.2.3")
}

func TestServerDiscoverIsModernOnly(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, "testdata/petstore.openapi.yaml")
	requestBody := []byte(`{"jsonrpc":"2.0","id":7,"method":"server/discover","params":{}}`)

	response := server.mcpRequestHandler(context.Background(), requestBody, mcpRequestHeaders{})
	immediate := requireImmediateResponse(t, response.Immediate)

	assert.Equal(t, typev3.StatusCode_OK, immediate.GetStatus().GetCode())
	assertJSONRPCErrorBody(t, immediate.GetBody(), jsonrpc.CodeMethodNotFound)
}
