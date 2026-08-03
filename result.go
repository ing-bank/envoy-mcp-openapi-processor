package envoy_mcp_openapi_processor

import (
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// resultTypeComplete is the only result type this server produces. It answers
// from the upstream API and never asks the client for further input.
const resultTypeComplete = "complete"

// resultMeta holds the envelope fields the 2026-07-28 revision defines on every
// result. Both are omitempty, so a result the legacy era stamps nothing on
// encodes as it did before the era split.
type resultMeta struct {
	Meta       map[string]any `json:"_meta,omitempty"`
	ResultType string         `json:"resultType,omitempty"`
}

// callToolResult mirrors [mcp.CallToolResult] minus the members a proxy never
// produces. It is ours rather than the sdk's because the sdk seals resultType
// behind an unexported field and a custom MarshalJSON.
type callToolResult struct {
	resultMeta
	Content []textContent `json:"content"`
	// StructuredContent is the upstream response body, forwarded verbatim when
	// the tool declares an output schema.
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
}

// listToolsResult mirrors [mcp.ListToolsResult] because the sdk
// embeds [mcp.Cacheable] without omitempty and cannot express "no cache hints"
// which we need to omit when responding to pre-2026 clients.
type listToolsResult struct {
	resultMeta
	*mcp.Cacheable
	Tools []*mcp.Tool `json:"tools"`
}

// textContent is the only content kind this server emits. Every tool result is
// an upstream HTTP response body, reported as text.
type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func newTextContent(text string) []textContent {
	return []textContent{{Type: "text", Text: text}}
}
