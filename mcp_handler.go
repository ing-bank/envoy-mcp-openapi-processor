package envoy_mcp_openapi_processor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

type callToolRequestParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type errorInfo struct {
	traceMessage string
}

// Custom attribute keys for OpenTelemetry spans
const (
	attrBodySize                       = "body.size"
	attrJSONRPCID                      = "jsonrpc.id"
	attrJSONRPCMethod                  = "jsonrpc.method"
	attrJSONRPCDecodeError             = "jsonrpc.decode.error"
	attrMCPMessageType                 = "mcp.message_type"
	attrMCPMethod                      = "mcp.method"
	attrMCPProtocolVersion             = "mcp.protocol_version"
	attrToolName                       = "tool.name"
	attrToolIsError                    = "tool.is_error"
	attrToolErrorReason                = "tool.error.reason"
	attrResponseMarshalError           = "response.marshal.error"
	attrResponseInvalidJson            = "response.invalid_json"
	attrResponseJSONRPCIDPresent       = "response.jsonrpcid_present"
	attrResponseToolCallContextPresent = "response.tool_call_ctx_present"
	attrResponseUseStructuredOutput    = "response.use_structured_output"
)

const (
	methodInitialize               = "initialize"
	methodToolsList                = "tools/list"
	methodToolsCall                = "tools/call"
	methodNotificationsInitialized = "notifications/initialized"
	methodDiscover                 = "server/discover"
)

type mcpProcResponse struct {
	Id jsonrpc.ID
	// Exactly one of Immediate / Reroute is set. Immediate holds verbatim
	// short-circuit responses (immediate body / status) sent as-is. Reroute holds
	// a streamed request-body rewrite that the transport layer frames into wire
	// messages (chunking, end-of-stream, trailers).
	Immediate []*extProcPb.ProcessingResponse
	Reroute   *streamedMutation
	// ToolResponseConfig is set alongside Reroute and describes how to translate
	// the upstream response back into an MCP envelope.
	ToolResponseConfig *toolResponseConfig
	// Era is set alongside Reroute, so the response phase need not re-derive it.
	Era protocolEra
}

func isHTTPErrorStatus(statusCode int) bool {
	return statusCode == 0 || statusCode >= 400
}

func findHeader(headers *corev3.HeaderMap, key string) (string, bool) {
	for _, h := range headers.GetHeaders() {
		if h.GetKey() == key {
			return string(h.GetRawValue()), true
		}
	}
	return "", false
}

func parseUpstreamStatusCode(headers *corev3.HeaderMap) (int, bool) {
	value, ok := findHeader(headers, ":status")
	if !ok {
		return 0, false
	}
	statusCode, err := strconv.Atoi(value)
	if err != nil {
		return 0, false
	}
	return statusCode, true
}

func encodeJSONRPCError(id jsonrpc.ID, code int64, message string) []byte {
	return encodeJSONRPCErrorWithData(id, code, message, nil)
}

func encodeJSONRPCErrorWithData(id jsonrpc.ID, code int64, message string, data any) []byte {
	jsonrpcError := &jsonrpc.Error{
		Code:    code,
		Message: message,
	}
	if data != nil {
		dataJSON, err := json.Marshal(data)
		if err != nil {
			zap.L().Error("Failed to marshal JSON-RPC error data, omitting it", zap.Error(err))
		} else {
			jsonrpcError.Data = dataJSON
		}
	}
	buf, err := jsonrpc.EncodeMessage(&jsonrpc.Response{
		ID:    id,
		Error: jsonrpcError,
	})
	if err != nil {
		zap.L().Error("Failed to encode JSON-RPC error response", zap.Error(err))
		return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":null,"error":{"code":%d,"message":"Internal error"}}`, jsonrpc.CodeInternalError))
	}
	return buf
}

func errorResponse(span trace.Span, era protocolEra, id jsonrpc.ID, code int64, message string, data any) *mcpProcResponse {
	span.SetStatus(codes.Error, message)
	return &mcpProcResponse{
		Id:        id,
		Immediate: newImmediateBodyResponseWithStatus(encodeJSONRPCErrorWithData(id, code, message, data), era.errorStatus(code)),
	}
}

func resultResponse(span trace.Span, era protocolEra, id jsonrpc.ID, result any) *mcpProcResponse {
	resultJSON, err := era.encodeResult(result)
	if err != nil {
		zap.L().Error("Error marshaling JSON-RPC result", zap.Error(err))
		span.RecordError(err)
		return errorResponse(span, era, id, jsonrpc.CodeInternalError, "Internal error", nil)
	}
	message, err := jsonrpc.EncodeMessage(&jsonrpc.Response{
		ID:     id,
		Result: resultJSON,
	})
	if err != nil {
		zap.L().Error("Error encoding JSON-RPC response", zap.Error(err))
		span.RecordError(err)
		return errorResponse(span, era, id, jsonrpc.CodeInternalError, "Internal error", nil)
	}
	return &mcpProcResponse{Id: id, Immediate: newImmediateBodyResponse(message)}
}

// toolExecutionErrorResponse reports a failure to build the upstream call as an
// MCP tool error rather than a JSON-RPC error, so the client sees a tool outcome.
func toolExecutionErrorResponse(span trace.Span, era protocolEra, id jsonrpc.ID, clientMessage string, traceReason string) *mcpProcResponse {
	if traceReason == "" {
		traceReason = clientMessage
	}
	span.SetAttributes(
		attribute.Bool(attrToolIsError, true),
		attribute.String(attrToolErrorReason, traceReason),
	)
	return resultResponse(span, era, id, &callToolResult{
		Content: newTextContent(clientMessage),
		IsError: true,
	})
}

func (s *extProcServer) mcpRequestHandler(ctx context.Context, body []byte, hdrs mcpRequestHeaders) *mcpProcResponse {
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(attribute.Int(attrBodySize, len(body)))

	// A message that fails to decode has no era, so both refusals below use the
	// legacy envelope, which returns them in a 200 body.
	msg, err := jsonrpc.DecodeMessage(body)
	if err != nil {
		span.RecordError(err)
		span.SetAttributes(attribute.String(attrJSONRPCDecodeError, err.Error()))
		return errorResponse(span, legacyEra{}, jsonrpc.ID{}, jsonrpc.CodeParseError, "Parse error", nil)
	}
	req, ok := msg.(*jsonrpc.Request)
	if !ok {
		span.SetAttributes(attribute.String(attrMCPMessageType, "not_request"))
		return errorResponse(span, legacyEra{}, jsonrpc.ID{}, jsonrpc.CodeInvalidRequest, "Invalid Request", nil)
	}
	span.SetAttributes(
		semconv.RPCMethod(req.Method),
		attribute.String(attrJSONRPCID, fmt.Sprint(req.ID.Raw())),
		attribute.String(attrJSONRPCMethod, req.Method),
		attribute.String(attrMCPMethod, mcpMethodLabel(req.Method)),
	)

	declared := metaProtocolVersion(req.Params)
	era, requestedVersion, ok := s.resolveEra(req.Method, declared, hdrs.protocolVersion)
	if requestedVersion != "" {
		span.SetAttributes(attribute.String(attrMCPProtocolVersion, protocolVersionLabel(requestedVersion)))
	}
	if !ok {
		return errorResponse(span, era, req.ID, mcp.CodeUnsupportedProtocolVersion,
			fmt.Sprintf("Unsupported protocol version: %s", requestedVersion),
			map[string]any{"supported": supportedProtocolVersions, "requested": requestedVersion})
	}
	if !era.serves(req.Method) {
		return errorResponse(span, era, req.ID, jsonrpc.CodeMethodNotFound, "Method not found", nil)
	}
	if err := era.validateHeaders(req, declared, hdrs); err != nil {
		return errorResponse(span, era, req.ID, mcp.CodeHeaderMismatch, err.Error(), nil)
	}

	switch req.Method {
	case methodInitialize:
		return s.handleInitialize(span, era, req)
	case methodNotificationsInitialized:
		return &mcpProcResponse{Id: req.ID, Immediate: httpStatusResponse(typev3.StatusCode_Accepted)}
	case methodDiscover:
		return s.handleDiscover(span, era, req)
	case methodToolsList:
		return resultResponse(span, era, req.ID, &listToolsResult{Tools: s.registry.Tools()})
	case methodToolsCall:
		return s.handleToolCall(span, era, req, hdrs.paramHeaderNames)
	default:
		return errorResponse(span, era, req.ID, jsonrpc.CodeMethodNotFound, "Method not found", nil)
	}
}

func protocolVersionLabel(version string) string {
	if isSupportedVersion(version) {
		return version
	}
	return "unsupported"
}

// handleDiscover answers the modern replacement for the initialize handshake.
// The envelope around the payload is the era's to stamp (see
// [modernEra.encodeResult]).
func (s *extProcServer) handleDiscover(span trace.Span, era protocolEra, req *jsonrpc.Request) *mcpProcResponse {
	return resultResponse(span, era, req.ID, &mcp.DiscoverResult{
		SupportedVersions: supportedProtocolVersions,
		Capabilities:      serverCapabilities(),
		Instructions:      s.serverInfo.Instructions,
	})
}

// serverCapabilities advertises tools only, since an OpenAPI document has
// nothing to project onto resources or prompts. Both discovery answers use it,
// and each gets a fresh value because the caller owns it.
func serverCapabilities() *mcp.ServerCapabilities {
	return &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}}
}

// mcpMethodLabel returns the method itself for any method some era serves and
// "unknown" otherwise. Deriving it from the eras keeps it in step with them.
func mcpMethodLabel(method string) string {
	if (legacyEra{}).serves(method) || (modernEra{}).serves(method) {
		return method
	}
	return "unknown"
}

func (s *extProcServer) handleInitialize(span trace.Span, era protocolEra, req *jsonrpc.Request) *mcpProcResponse {
	var initializeParams mcp.InitializeParams
	if err := json.Unmarshal(req.Params, &initializeParams); err != nil {
		zap.L().Debug("Error unmarshalling initialize request", zap.Error(err))
		span.RecordError(err)
		return errorResponse(span, era, req.ID, jsonrpc.CodeInvalidParams, fmt.Sprintf("Invalid params: %v", err), nil)
	}
	negotiatedProtocolVersion := negotiateLegacyVersion(initializeParams.ProtocolVersion)
	return resultResponse(span, era, req.ID, &mcp.InitializeResult{
		ProtocolVersion: negotiatedProtocolVersion,
		ServerInfo: &mcp.Implementation{
			Name:    s.serverInfo.Name,
			Version: s.serverInfo.Version,
		},
		Capabilities: serverCapabilities(),
		Instructions: s.serverInfo.Instructions,
	})
}

func (s *extProcServer) handleToolCall(span trace.Span, era protocolEra, req *jsonrpc.Request, stripHeaders []string) *mcpProcResponse {
	var callParams callToolRequestParams
	if err := json.Unmarshal(req.Params, &callParams); err != nil {
		zap.L().Error("Error unmarshaling tool call request", zap.Error(err))
		span.RecordError(err)
		return errorResponse(span, era, req.ID, jsonrpc.CodeInvalidParams, fmt.Sprintf("Invalid params: %v", err), nil)
	}
	span.SetAttributes(attribute.String(attrToolName, callParams.Name))

	toolConfig := s.registry.GetConfig(callParams.Name)
	if toolConfig == nil {
		return errorResponse(span, era, req.ID, jsonrpc.CodeInvalidParams, fmt.Sprintf("Unknown tool: %s", callParams.Name), nil)
	}

	endpointReq, paramErr := newEndpointRequest(toolConfig.Endpoint, callParams.Arguments)
	if paramErr != nil {
		clientMessage := fmt.Sprintf("Invalid argument %q: %s", paramErr.paramName, paramErr.reason)
		return toolExecutionErrorResponse(span, era, req.ID, clientMessage, fmt.Sprintf("Invalid parameter in=%s, name=%s: %s", paramErr.paramIn, paramErr.paramName, paramErr.reason))
	}

	path := endpointReq.fullPath()

	requestBody, err := marshalRequestBody(toolConfig.Endpoint, callParams.Arguments)
	if err != nil {
		zap.L().Error("Error marshaling request body", zap.Error(err))
		span.RecordError(err)
		return toolExecutionErrorResponse(span, era, req.ID, fmt.Sprintf("Failed to serialize 'body' argument: %v", err), "")
	}

	span.SetAttributes(
		semconv.HTTPRequestMethodKey.String(strings.ToUpper(toolConfig.Endpoint.Method)),
		semconv.HTTPRoute(path),
		semconv.HTTPRequestBodySize(len(requestBody)),
	)
	return &mcpProcResponse{
		Id:                 req.ID,
		Reroute:            rerouteWithBodyMutation(toolConfig.Endpoint.Host, strings.ToUpper(toolConfig.Endpoint.Method), path, requestBody, endpointReq.extraHeaders, stripHeaders),
		ToolResponseConfig: &toolConfig.toolResponseConfig,
		Era:                era,
	}
}

func buildMCPResponsePayload(ctx context.Context, reqCtx *requestContext, body []byte) ([]byte, []*corev3.HeaderValueOption) {
	span := trace.SpanFromContext(ctx)

	buf, err := buildMCPResponse(ctx, reqCtx, body, httpStatusToErrorInfo(reqCtx.upstreamStatusCode))
	if err != nil {
		span.RecordError(err)
		span.SetAttributes(attribute.String(attrResponseMarshalError, err.Error()))
		span.SetStatus(codes.Error, "failed to build MCP response")
		buf = encodeJSONRPCError(reqCtx.jsonrpcID, jsonrpc.CodeInternalError, "Internal error")
	}

	headers := appendHeader(nil, "content-type", "application/json")
	headers = appendHeader(headers, "content-length", strconv.Itoa(len(buf)))
	return buf, headers
}

// buildEndOfStreamResponse synthesizes an MCP response for a tool call whose
// upstream returned no body (end_of_stream on the response headers).
func buildEndOfStreamResponse(ctx context.Context, reqCtx *requestContext) []*extProcPb.ProcessingResponse {
	content := describeEmptyUpstreamResponse(reqCtx.upstreamStatusCode)

	buf, headers := buildMCPResponsePayload(ctx, reqCtx, []byte(content))
	return newReplacedResponse(headers, buf)
}

func describeEmptyUpstreamResponse(statusCode int) string {
	if statusCode == 0 {
		return "API returned an unknown status with no response body."
	}
	statusText := http.StatusText(statusCode)
	if statusText == "" {
		statusText = "Unknown Status"
	}
	return fmt.Sprintf("API returned HTTP %d (%s) with no response body.", statusCode, statusText)
}

func httpStatusToErrorInfo(statusCode int) *errorInfo {
	if !isHTTPErrorStatus(statusCode) {
		return nil
	}
	return &errorInfo{fmt.Sprintf("upstream returned error status code %d", statusCode)}
}

func mcpResponseHandler(ctx context.Context, body []byte, reqCtx *requestContext) streamedMutation {
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		attribute.Int(attrBodySize, len(body)),
		attribute.Bool(attrResponseToolCallContextPresent, reqCtx.toolResponseConfig != nil),
		attribute.Bool(attrResponseJSONRPCIDPresent, reqCtx.jsonrpcID.IsValid()),
	)
	if reqCtx.jsonrpcID.IsValid() {
		span.SetAttributes(attribute.String(attrJSONRPCID, fmt.Sprint(reqCtx.jsonrpcID.Raw())))
	}
	buf, headers := buildMCPResponsePayload(ctx, reqCtx, body)
	return streamedMutation{
		headers: responseFactory.headerMutation(headers, contentHeaders),
		body:    buf,
	}
}

func buildMCPResponse(ctx context.Context, reqCtx *requestContext, body []byte, errorInfo *errorInfo) ([]byte, error) {
	span := trace.SpanFromContext(ctx)
	isError := errorInfo != nil

	if !reqCtx.jsonrpcID.IsValid() {
		return nil, fmt.Errorf("missing req context information: jsonrpcid")
	}

	config := reqCtx.toolResponseConfig
	if config == nil {
		return nil, fmt.Errorf("missing req context information: tool config ref")
	}

	span.SetAttributes(attribute.Bool(attrResponseUseStructuredOutput, config.UseStructuredOutput))

	var structuredContent json.RawMessage
	if config.UseStructuredOutput {
		if !json.Valid(body) {
			span.SetAttributes(attribute.String(attrResponseInvalidJson, "upstream response body not valid JSON"))
		} else if !isError {
			// do not set structured content for errors, as outputSchema is defined for 2xx responses only
			structuredContent = body
		}
	}

	span.SetAttributes(attribute.Bool(attrToolIsError, isError))
	if isError {
		span.SetAttributes(attribute.String(attrToolErrorReason, errorInfo.traceMessage))
	}

	result := &callToolResult{
		Content:           newTextContent(string(body)),
		StructuredContent: structuredContent,
		IsError:           isError,
	}
	resultJson, err := reqCtx.protocolEra().encodeResult(result)
	if err != nil {
		return nil, err
	}
	message, err := jsonrpc.EncodeMessage(&jsonrpc.Response{
		ID:     reqCtx.jsonrpcID,
		Result: resultJson,
	})
	if err != nil {
		return nil, err
	}
	return message, nil
}
