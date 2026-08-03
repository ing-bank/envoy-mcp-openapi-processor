package envoy_mcp_openapi_processor

import (
	"encoding/json"
	"testing"

	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNegotiateLegacyVersion(t *testing.T) {
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
			want:          latestLegacyVersion,
		},
		{
			name:          "never negotiates a modern version on the legacy handshake",
			clientVersion: v20260728,
			want:          latestLegacyVersion,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, negotiateLegacyVersion(test.clientVersion))
		})
	}
}

func TestIsModernVersion(t *testing.T) {
	assert.False(t, isModernVersion(v20250618))
	assert.False(t, isModernVersion(v20251125))
	assert.False(t, isModernVersion(""))
	assert.True(t, isModernVersion(v20260728))
	assert.True(t, isModernVersion("2027-01-01"))
}

func TestResolveEra(t *testing.T) {
	server := &extProcServer{serverInfo: ServerInfo{Name: "srv", Version: "1"}}

	tests := []struct {
		name     string
		method   string
		declared string
		wantEra  protocolEra
		wantOK   bool
	}{
		{name: "no declared version is legacy", method: methodToolsList, wantEra: legacyEra{}, wantOK: true},
		{name: "initialize is legacy", method: methodInitialize, wantEra: legacyEra{}, wantOK: true},
		{name: "initialize ignores a declared version", method: methodInitialize, declared: v20260728, wantEra: legacyEra{}, wantOK: true},
		{name: "notifications/initialized is legacy", method: methodNotificationsInitialized, declared: v20260728, wantEra: legacyEra{}, wantOK: true},
		{name: "supported modern version", method: methodToolsList, declared: v20260728, wantEra: modernEra{}, wantOK: true},
		// A pre-cutover declaration is a legacy client restating its negotiated
		// version, so it is ignored and the request is served.
		{name: "pre-cutover version is ignored", method: methodToolsList, declared: v20251125, wantEra: legacyEra{}, wantOK: true},
		{name: "unsupported older version is ignored", method: methodToolsList, declared: "2024-11-05", wantEra: legacyEra{}, wantOK: true},
		// Past the cutover the declaration is binding, so an unknown version
		// there is refused rather than ignored.
		{name: "unknown future version is refused", method: methodToolsList, declared: "2099-01-01", wantEra: modernEra{}, wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			era, ok := server.resolveEra(tt.method, tt.declared)
			assert.IsType(t, tt.wantEra, era)
			assert.Equal(t, tt.wantOK, ok)
		})
	}
}

func TestProtocolEraMethodSurface(t *testing.T) {
	tests := []struct {
		method       string
		legacyServes bool
		modernServes bool
	}{
		{method: methodToolsList, legacyServes: true, modernServes: true},
		{method: methodToolsCall, legacyServes: true, modernServes: true},
		// The handshake exists only in the legacy era.
		{method: methodInitialize, legacyServes: true, modernServes: false},
		{method: methodNotificationsInitialized, legacyServes: true, modernServes: false},
		{method: "resources/list", legacyServes: false, modernServes: false},
	}

	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			assert.Equal(t, tt.legacyServes, legacyEra{}.serves(tt.method), "legacy")
			assert.Equal(t, tt.modernServes, modernEra{}.serves(tt.method), "modern")
		})
	}
}

func TestProtocolEraErrorStatus(t *testing.T) {
	tests := []struct {
		name       string
		code       int64
		wantLegacy typev3.StatusCode
		wantModern typev3.StatusCode
	}{
		{name: "unsupported protocol version", code: mcp.CodeUnsupportedProtocolVersion, wantLegacy: typev3.StatusCode_OK, wantModern: typev3.StatusCode_BadRequest},
		{name: "method not found", code: jsonrpc.CodeMethodNotFound, wantLegacy: typev3.StatusCode_OK, wantModern: typev3.StatusCode_NotFound},
		{name: "invalid params", code: jsonrpc.CodeInvalidParams, wantLegacy: typev3.StatusCode_OK, wantModern: typev3.StatusCode_BadRequest},
		{name: "parse error", code: jsonrpc.CodeParseError, wantLegacy: typev3.StatusCode_OK, wantModern: typev3.StatusCode_OK},
		{name: "internal error", code: jsonrpc.CodeInternalError, wantLegacy: typev3.StatusCode_OK, wantModern: typev3.StatusCode_OK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantLegacy, legacyEra{}.errorStatus(tt.code), "legacy")
			assert.Equal(t, tt.wantModern, modernEra{}.errorStatus(tt.code), "modern")
		})
	}
}

func TestProtocolEraEncodeResult(t *testing.T) {
	// The modern era stamps the value it is handed, so each era gets a fresh
	// result; a shared one would make the assertions order-dependent.
	tests := []struct {
		name      string
		newResult func() any
	}{
		{name: "tools/call", newResult: func() any {
			return &callToolResult{Content: newTextContent("hi")}
		}},
		{name: "tools/list", newResult: func() any {
			return &mcp.ListToolsResult{Tools: []*mcp.Tool{{Name: "tool"}}}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			legacyJSON, err := legacyEra{}.encodeResult(tt.newResult())
			require.NoError(t, err)
			var legacyObj map[string]any
			require.NoError(t, json.Unmarshal(legacyJSON, &legacyObj))
			assert.NotContains(t, legacyObj, "resultType")
			assert.NotContains(t, legacyObj, "_meta")

			era := modernEra{serverInfo: ServerInfo{Name: "srv", Version: "1.2.3"}}
			modernJSON, err := era.encodeResult(tt.newResult())
			require.NoError(t, err)
			var modernObj map[string]any
			require.NoError(t, json.Unmarshal(modernJSON, &modernObj))
			assert.Equal(t, "complete", modernObj["resultType"])
			assertServerInfoMeta(t, modernObj, "srv", "1.2.3")
		})
	}
}

func TestModernEraEncodeResultKeepsExistingMeta(t *testing.T) {
	result := &callToolResult{
		Content:    newTextContent("hi"),
		resultMeta: resultMeta{Meta: map[string]any{"vendor/key": "kept"}},
	}

	era := modernEra{serverInfo: ServerInfo{Name: "srv", Version: "1.2.3"}}
	modernJSON, err := era.encodeResult(result)
	require.NoError(t, err)

	var obj map[string]any
	require.NoError(t, json.Unmarshal(modernJSON, &obj))
	assert.Equal(t, "complete", obj["resultType"])
	assertServerInfoMeta(t, obj, "srv", "1.2.3")
	meta, ok := obj["_meta"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "kept", meta["vendor/key"])
}

// TestModernEraEncodeResultRefusesUnstampable also covers nil. Every result is
// a pointer, so a nil one still carries its type into the interface and only
// fails when dereferenced.
func TestModernEraEncodeResultRefusesUnstampable(t *testing.T) {
	era := modernEra{serverInfo: ServerInfo{Name: "srv", Version: "1.2.3"}}

	tests := []struct {
		name   string
		result any
	}{
		{name: "untyped nil", result: nil},
		{name: "nil tools/call result", result: (*callToolResult)(nil)},
		{name: "nil tools/list result", result: (*mcp.ListToolsResult)(nil)},
		// initialize is legacy-only, so its result never reaches this era.
		{name: "result of a method the era does not serve", result: &mcp.InitializeResult{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := era.encodeResult(tt.result)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "cannot encode result")
		})
	}
}

// TestCallToolResultDecodesAsSDKResult is why the sdk stays a dependency: it
// validates the bytes we emit rather than producing them.
func TestCallToolResultDecodesAsSDKResult(t *testing.T) {
	era := modernEra{serverInfo: ServerInfo{Name: "srv", Version: "1.2.3"}}
	encoded, err := era.encodeResult(&callToolResult{
		Content:           newTextContent("body"),
		StructuredContent: json.RawMessage(`{"count":42}`),
		IsError:           true,
	})
	require.NoError(t, err)

	var decoded mcp.CallToolResult
	require.NoError(t, json.Unmarshal(encoded, &decoded))

	require.Len(t, decoded.Content, 1)
	text, ok := decoded.Content[0].(*mcp.TextContent)
	require.True(t, ok, "content should decode as text content")
	assert.Equal(t, "body", text.Text)
	assert.True(t, decoded.IsError)
	// Decoded through any, so the count arrives as a float64.
	assert.Equal(t, map[string]any{"count": float64(42)}, decoded.StructuredContent)
	assertServerInfoMeta(t, map[string]any{"_meta": map[string]any(decoded.GetMeta())}, "srv", "1.2.3")
}

func TestMetaProtocolVersion(t *testing.T) {
	tests := []struct {
		name   string
		params string
		want   string
	}{
		{name: "declared version", params: `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}`, want: v20260728},
		{name: "no params", params: ``},
		{name: "no meta", params: `{"name":"x"}`},
		{name: "meta without the version key", params: `{"_meta":{"other":"x"}}`},
		{name: "non-string version", params: `{"_meta":{"io.modelcontextprotocol/protocolVersion":42}}`},
		{name: "meta is not an object", params: `{"_meta":"bad"}`},
		{name: "params are not an object", params: `"bad"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, metaProtocolVersion(json.RawMessage(tt.params)))
		})
	}
}
