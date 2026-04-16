package envoy_mcp_openapi_processor

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

// newFuzzServer creates a minimal extProcServer suitable for fuzz testing.
// It loads the petstore spec so tools/call paths exercise real tool routing.
func newFuzzServer(t testing.TB) *extProcServer {
	t.Helper()
	registry, err := newToolRegistryFromConfig(&ToolRegistryConfig{OpenAPISpecPattern: "testdata/petstore.openapi.yaml"})
	if err != nil {
		t.Fatalf("failed to load test tools: %v", err)
	}
	return &extProcServer{
		registry: registry,
	}
}

func FuzzMcpRequestHandler(f *testing.F) {
	// Seed corpus — valid requests
	f.Add([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	f.Add([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`))
	f.Add([]byte(`{"jsonrpc":"2.0","id":3,"method":"notifications/initialized","params":{}}`))
	f.Add([]byte(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"findPetsByStatus","arguments":{"status":"available"}}}`))
	f.Add([]byte(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"getPetById","arguments":{"petId":"42"}}}`))
	f.Add([]byte(`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"addPet","arguments":{"body":{"name":"doggo","photoUrls":["http://example.com"]}}}}`))
	f.Add([]byte(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"updatePet","arguments":{"body":{"id":1,"name":"kitty"}}}}`))
	f.Add([]byte(`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"nonExistentTool","arguments":{}}}`))
	f.Add([]byte(`{"jsonrpc":"2.0","id":9,"method":"unknown/method","params":{}}`))

	// Seed corpus — malformed / edge-case inputs
	f.Add([]byte(``))
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`"just a string"`))
	f.Add([]byte(`42`))
	f.Add([]byte(`{invalid json`))
	f.Add([]byte("\x00\x01\x02\x03"))
	f.Add([]byte(`{"jsonrpc":"2.0","method":"initialize"}`))
	f.Add([]byte(`{"jsonrpc":"2.0","id":null,"method":"initialize"}`))
	f.Add([]byte(`{"jsonrpc":"2.0","id":"str","method":"initialize"}`))
	f.Add([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call"}`))
	f.Add([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"bad"}`))
	f.Add([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"","arguments":{}}}`))
	f.Add([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"arguments":{"a":"b"}}}`))
	f.Add([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":123,"arguments":{}}}`))
	// Deeply nested JSON objects: {"a":{"a":{"a":...}}}
	deepNest := strings.Repeat(`{"a":`, 5000) + `true` + strings.Repeat(`}`, 5000)
	f.Add([]byte(deepNest))
	// Deeply nested structure inside a valid JSON-RPC tools/call envelope
	f.Add([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"addPet","arguments":{"body":` + deepNest + `}}}`))
	// Very long method name
	f.Add([]byte(`{"jsonrpc":"2.0","id":1,"method":"` + strings.Repeat("m", 100000) + `","params":{}}`))

	server := newFuzzServer(f)

	f.Fuzz(func(t *testing.T, data []byte) {
		ctx := context.Background()
		// Must not panic regardless of input
		server.mcpRequestHandler(ctx, data)
	})
}

func FuzzMcpResponseHandler(f *testing.F) {
	// Seed corpus — valid upstream response bodies
	f.Add([]byte(`{"result":"success","data":{"key":"value"}}`), "1")
	f.Add([]byte(`{}`), "2")
	f.Add([]byte(`{"items":[1,2,3]}`), "3")
	f.Add([]byte(`{"a":null}`), "4")

	// Seed corpus — edge cases
	f.Add([]byte(``), "5")
	f.Add([]byte(`null`), "6")
	f.Add([]byte(`[]`), "7")
	f.Add([]byte(`"string"`), "8")
	f.Add([]byte(`{broken`), "9")
	f.Add([]byte("\x00\xff"), "10")
	f.Add([]byte(`{"deeply":{"nested":{"object":{"value":true}}}}`), "11")

	server := newFuzzServer(f)

	f.Fuzz(func(t *testing.T, body []byte, jsonrpcIDStr string) {
		ctx := context.Background()
		jsonrpcID, _ := jsonrpc.MakeID(jsonrpcIDStr)
		// Must not panic regardless of input
		server.mcpResponseHandler(ctx, body, jsonrpcID, &toolResponseConfig{}, nil)
	})
}
