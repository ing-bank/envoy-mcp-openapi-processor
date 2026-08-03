package envoy_mcp_openapi_processor

import (
	"encoding/json"
	"fmt"
	"slices"

	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	v20250618 = "2025-06-18"
	v20251125 = "2025-11-25"
	v20260728 = "2026-07-28"
	// firstModernVersion is the first revision that drops the handshake in favor
	// of a protocol version on every request; everything before it is legacy.
	firstModernVersion = v20260728
	// latestLegacyVersion is the newest version that still uses the handshake.
	latestLegacyVersion = v20251125
)

// supportedProtocolVersions is ordered newest first, which is the order clients
// see when a version is refused.
var supportedProtocolVersions = []string{
	v20260728,
	v20251125,
	v20250618,
}

// isModernVersion works because versions are ISO dates, so lexicographic order
// is chronological. The empty string sorts below every real version.
func isModernVersion(v string) bool {
	return v >= firstModernVersion
}

func isSupportedVersion(v string) bool {
	return slices.Contains(supportedProtocolVersions, v)
}

// negotiateLegacyVersion honors the client's version when supported, otherwise
// falls back to our newest (adopted from modelcontextprotocol/go-sdk). Both
// outcomes are capped at the legacy era, so a client that asks for a modern
// version over the handshake gets one it can actually use statefully.
func negotiateLegacyVersion(clientVersion string) string {
	if isSupportedVersion(clientVersion) && !isModernVersion(clientVersion) {
		return clientVersion
	}
	return latestLegacyVersion
}

// protocolEra holds the behavior that differs between generations of the MCP
// spec. [extProcServer.resolveEra] turns a request's version into an era once,
// and the rest of the code asks the era instead of comparing versions.
type protocolEra interface {
	// serves gates dispatch. A method only another era serves is a "method not
	// found" here, not an unimplemented one.
	serves(method string) bool
	// encodeResult marshals result into this era's JSON-RPC result envelope,
	// stamping the fields the generation makes mandatory.
	encodeResult(result any) (json.RawMessage, error)
	errorStatus(code int64) typev3.StatusCode
	validateHeaders(req *jsonrpc.Request, metaVersion string, hdrs mcpRequestHeaders) error
}

// legacyEra serves every revision up to and including [latestLegacyVersion].
// The client negotiates a version over the initialize handshake, results are
// plain JSON-RPC, and protocol errors are returned in a 200 response body.
type legacyEra struct{}

func (legacyEra) serves(method string) bool {
	switch method {
	case methodInitialize, methodNotificationsInitialized, methodToolsList, methodToolsCall:
		return true
	default:
		return false
	}
}

// encodeResult marshals the result as-is: every modern envelope field encodes as
// absent when unset (see [resultMeta] and [listToolsResult]).
func (legacyEra) encodeResult(result any) (json.RawMessage, error) {
	return json.Marshal(result)
}

// errorStatus is always 200. The legacy era returns every JSON-RPC error,
// protocol errors included, in the body of a successful HTTP response.
func (legacyEra) errorStatus(int64) typev3.StatusCode {
	return typev3.StatusCode_OK
}

// validateHeaders accepts anything. The mirrored MCP request headers are a
// 2026-07-28 addition, so a legacy client is not required to send them.
func (legacyEra) validateHeaders(*jsonrpc.Request, string, mcpRequestHeaders) error {
	return nil
}

// modernEra serves 2026-07-28 and later. There is no handshake, so the method
// surface shrinks to the tool calls, and every result is stamped with the
// revision's envelope fields (changelog Major 2 and Major 8).
type modernEra struct {
	serverInfo ServerInfo
}

// Cache hints for the results the revision makes cacheable (changelog Minor 5).
// Both are fixed. Five minutes bounds how long a client may keep using a
// previous deployment's tool list, since a redeploy is the only way it changes
// and an ext_proc filter has no channel to announce one. "private" keeps the
// response out of shared caches it never travels through anyway.
const (
	listCacheTTLMillis = 300000
	listCacheScope     = "private"
)

func (modernEra) serves(method string) bool {
	switch method {
	case methodToolsList, methodToolsCall:
		return true
	default:
		return false
	}
}

// encodeResult stamps the 2026-07-28 envelope fields onto the results the
// methods in [modernEra.serves] produce: the required resultType, the serverInfo
// _meta entry servers SHOULD add, and the cache hints on cacheable results.
func (e modernEra) encodeResult(result any) (json.RawMessage, error) {
	switch r := result.(type) {
	case *callToolResult:
		if r == nil {
			break
		}
		e.stampEnvelope(&r.resultMeta)
		return json.Marshal(r)
	case *listToolsResult:
		if r == nil {
			break
		}
		e.stampEnvelope(&r.resultMeta)
		// SEP-2549 makes a tool list cacheable; a tool call's result is not.
		r.Cacheable = &mcp.Cacheable{TTLMs: listCacheTTLMillis, CacheScope: listCacheScope}
		return json.Marshal(r)
	}
	return nil, fmt.Errorf("modern era cannot encode result %T", result)
}

// stampEnvelope writes the fields every modern result carries: the serverInfo
// _meta entry (SEP-2575) and resultType (SEP-2322).
func (e modernEra) stampEnvelope(m *resultMeta) {
	m.Meta = e.metaWithServerInfo(m.Meta)
	m.ResultType = resultTypeComplete
}

// metaWithServerInfo adds the serverInfo entry (SEP-2575), leaving the rest.
func (e modernEra) metaWithServerInfo(meta map[string]any) map[string]any {
	if meta == nil {
		meta = map[string]any{}
	}
	meta[mcp.MetaKeyServerInfo] = &mcp.Implementation{
		Name:    e.serverInfo.Name,
		Version: e.serverInfo.Version,
	}
	return meta
}

// errorStatus gives the protocol errors the statuses SEP-2575 mandates. Every
// other JSON-RPC error is an application error and is returned in a 200 body,
// as in the legacy era.
func (modernEra) errorStatus(code int64) typev3.StatusCode {
	switch code {
	case mcp.CodeUnsupportedProtocolVersion, mcp.CodeHeaderMismatch, jsonrpc.CodeInvalidParams:
		return typev3.StatusCode_BadRequest
	case jsonrpc.CodeMethodNotFound:
		return typev3.StatusCode_NotFound
	default:
		return typev3.StatusCode_OK
	}
}

// validateHeaders enforces SEP-2243 (see [validateModernHeaders]).
func (modernEra) validateHeaders(req *jsonrpc.Request, metaVersion string, hdrs mcpRequestHeaders) error {
	return validateModernHeaders(req, metaVersion, hdrs)
}

// resolveEra also reports the version that selected the era and whether the
// request may be served. Past the cutover an unsupported version is refused
// rather than ignored.
func (s *extProcServer) resolveEra(method string, declared string, mirrored string) (protocolEra, string, bool) {
	if method == methodInitialize || method == methodNotificationsInitialized {
		return legacyEra{}, "", true
	}
	requested := declared
	if !isModernVersion(requested) {
		requested = mirrored
	}
	if !isModernVersion(requested) {
		return legacyEra{}, "", true
	}
	return modernEra{serverInfo: s.serverInfo}, requested, isSupportedVersion(requested)
}

// metaProtocolVersion returns "" when the entry is absent or malformed.
func metaProtocolVersion(params json.RawMessage) string {
	if len(params) == 0 {
		return ""
	}
	var peek struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(params, &peek); err != nil {
		return ""
	}
	raw, ok := peek.Meta[mcp.MetaKeyProtocolVersion]
	if !ok {
		return ""
	}
	var version string
	if err := json.Unmarshal(raw, &version); err != nil {
		return ""
	}
	return version
}
