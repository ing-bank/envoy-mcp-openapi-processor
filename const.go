package envoy_mcp_openapi_processor

const (
	componentName = "envoy-mcp-openapi-processor"

	kb = 1024

	// streamChunkSize is the maximum chunk size for body streaming
	streamChunkSize = 60 * kb

	// initialBodyBufCap is the starting capacity of the per-stream body buffer
	initialBodyBufCap = 6 * kb
)
