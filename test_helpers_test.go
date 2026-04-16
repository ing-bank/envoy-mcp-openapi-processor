package envoy_mcp_openapi_processor

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

// mustMakeID creates a jsonrpc.ID from the given value or panics.
func mustMakeID(v any) jsonrpc.ID {
	id, err := jsonrpc.MakeID(v)
	if err != nil {
		panic(err)
	}
	return id
}

// newTestServer creates a properly configured extProcServer for testing.
func newTestServer(t *testing.T, openAPIPath string) *extProcServer {
	t.Helper()
	registry, err := newToolRegistryFromConfig(&ToolRegistryConfig{OpenAPISpecPattern: openAPIPath})
	if err != nil {
		t.Fatalf("Warning: failed to load tools config: %v", err)
	}
	return &extProcServer{registry: registry}
}
