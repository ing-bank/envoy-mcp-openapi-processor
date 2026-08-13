package envoy_mcp_openapi_processor

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

// MCP Streamable HTTP header names, lowercase as delivered by Envoy.
const (
	headerMCPProtocolVersion = "mcp-protocol-version"
	headerMCPMethod          = "mcp-method"
	headerMCPName            = "mcp-name"
	headerMCPParamPrefix     = "mcp-param-"
)

// Base64 sentinel wrapper, per SEP-2243.
const (
	base64Prefix = "=?base64?"
	base64Suffix = "?="
)

// mcpRequestHeaders carries the headers captured at the request-headers phase
// into the body phase, where the modern era validates them against the body. An
// absent header is the empty string.
type mcpRequestHeaders struct {
	protocolVersion string
	method          string
	name            string
	// paramHeaderNames lists the mcp-param-* header keys present on the
	// request, so the reroute path can strip them before forwarding upstream.
	paramHeaderNames []string
}

// captureMCPRequestHeaders keeps the first value of a repeated header.
// http.Header.Get returns the first, so that is the value every other hop reads;
// keeping the last would let a client pass validation here on a value the
// previous hop did not see.
func captureMCPRequestHeaders(headers *corev3.HeaderMap) mcpRequestHeaders {
	var captured mcpRequestHeaders
	var protocolVersion, method, name firstHeaderValue
	for _, h := range headers.GetHeaders() {
		switch key := strings.ToLower(h.GetKey()); key {
		case headerMCPProtocolVersion:
			protocolVersion.capture(h)
		case headerMCPMethod:
			method.capture(h)
		case headerMCPName:
			name.capture(h)
		default:
			if strings.HasPrefix(key, headerMCPParamPrefix) {
				captured.paramHeaderNames = append(captured.paramHeaderNames, key)
			}
		}
	}
	captured.protocolVersion = protocolVersion.value
	captured.method = method.value
	captured.name = name.value
	return captured
}

// firstHeaderValue keeps the first value seen, empty ones included: an empty
// first value is what Get returns too.
type firstHeaderValue struct {
	value string
	seen  bool
}

func (f *firstHeaderValue) capture(h *corev3.HeaderValue) {
	if f.seen {
		return
	}
	f.value, f.seen = headerValue(h), true
}

func headerValue(h *corev3.HeaderValue) string {
	return strings.TrimSpace(string(h.GetRawValue()))
}

// callToolNameParams reads the same `name` as [callToolRequestParams]. Decoding
// into that type here would materialize the arguments, which hold a tool call's
// request body, a second time.
type callToolNameParams struct {
	Name string `json:"name"`
}

// validateModernHeaders enforces SEP-2243: the mirrored headers must be present
// and match the body. metaVersion comes from params._meta, empty if absent.
func validateModernHeaders(req *jsonrpc.Request, metaVersion string, hdrs mcpRequestHeaders) error {
	if hdrs.protocolVersion == "" {
		return errors.New("missing required MCP-Protocol-Version header")
	}
	// The body is the source of truth and every 2026-07-28 request carries its
	// version in _meta, so a header with nothing to match is a mismatch.
	if metaVersion == "" {
		return fmt.Errorf("header mismatch: MCP-Protocol-Version header value '%s' has no matching protocolVersion in the body _meta", hdrs.protocolVersion)
	}
	if hdrs.protocolVersion != metaVersion {
		return fmt.Errorf("header mismatch: MCP-Protocol-Version header value '%s' does not match body value '%s'", hdrs.protocolVersion, metaVersion)
	}
	if hdrs.method == "" {
		return errors.New("missing required Mcp-Method header")
	}
	if hdrs.method != req.Method {
		return fmt.Errorf("header mismatch: Mcp-Method header value '%s' does not match body value '%s'", hdrs.method, req.Method)
	}
	if req.Method == methodToolsCall {
		if hdrs.name == "" {
			return fmt.Errorf("missing required Mcp-Name header for method %q", req.Method)
		}
		decodedName, ok := decodeHeaderValue(hdrs.name)
		if !ok {
			return errors.New("header mismatch: Mcp-Name header contains invalid Base64 encoding")
		}
		var params callToolNameParams
		if err := json.Unmarshal(req.Params, &params); err != nil || params.Name == "" {
			return fmt.Errorf("failed to extract name from parameters for method %q", req.Method)
		}
		// Report the decoded value, since a Base64-wrapped header cannot be
		// compared against the body value by eye.
		if decodedName != params.Name {
			return fmt.Errorf("header mismatch: Mcp-Name header value '%s' does not match body value '%s'", decodedName, params.Name)
		}
	}
	return nil
}

// decodeHeaderValue unwraps the SEP-2243 =?base64?...?= sentinel. Registered
// tool names never need it, since [validateToolName] keeps them header-safe, but
// a client may wrap anyway. Unwrapped values pass through; false means the
// wrapped payload is not valid Base64.
func decodeHeaderValue(headerValue string) (string, bool) {
	if encoded, ok := strings.CutPrefix(headerValue, base64Prefix); ok {
		if encoded, ok = strings.CutSuffix(encoded, base64Suffix); ok {
			decoded, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				return "", false
			}
			return string(decoded), true
		}
	}
	return headerValue, true
}
