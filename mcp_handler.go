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
	buf, err := jsonrpc.EncodeMessage(&jsonrpc.Response{
		ID: id,
		Error: &jsonrpc.Error{
			Code:    code,
			Message: message,
		},
	})
	if err != nil {
		zap.L().Error("Failed to encode JSON-RPC error response", zap.Error(err))
		return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":null,"error":{"code":%d,"message":"Internal error"}}`, jsonrpc.CodeInternalError))
	}
	return buf
}

func jsonrpcErrorImmediateResponse(span trace.Span, id jsonrpc.ID, code int64, message string) *mcpProcResponse {
	span.SetStatus(codes.Error, message)
	return &mcpProcResponse{Id: id, Immediate: newImmediateBodyResponse(encodeJSONRPCError(id, code, message))}
}

func toolExecutionErrorResponse(span trace.Span, id jsonrpc.ID, clientMessage string, traceReason string) *mcpProcResponse {
	if traceReason == "" {
		traceReason = clientMessage
	}
	span.SetAttributes(
		attribute.Bool(attrToolIsError, true),
		attribute.String(attrToolErrorReason, traceReason),
	)
	return jsonrpcImmediateResponse(span, id, &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: clientMessage},
		},
		IsError: true,
	})
}

func jsonrpcImmediateResponse(span trace.Span, id jsonrpc.ID, result any) *mcpProcResponse {
	resultJSON, err := json.Marshal(result)
	if err != nil {
		zap.L().Error("Error marshaling JSON-RPC result", zap.Error(err))
		span.RecordError(err)
		return jsonrpcErrorImmediateResponse(span, id, jsonrpc.CodeInternalError, "Internal error")
	}
	message, err := jsonrpc.EncodeMessage(&jsonrpc.Response{
		ID:     id,
		Result: resultJSON,
	})
	if err != nil {
		zap.L().Error("Error encoding JSON-RPC response", zap.Error(err))
		span.RecordError(err)
		return jsonrpcErrorImmediateResponse(span, id, jsonrpc.CodeInternalError, "Internal error")
	}
	return &mcpProcResponse{Id: id, Immediate: newImmediateBodyResponse(message)}
}

func (s *extProcServer) mcpRequestHandler(ctx context.Context, body []byte) *mcpProcResponse {
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(attribute.Int(attrBodySize, len(body)))

	msg, err := jsonrpc.DecodeMessage(body)
	if err != nil {
		span.RecordError(err)
		span.SetAttributes(attribute.String(attrJSONRPCDecodeError, err.Error()))
		return jsonrpcErrorImmediateResponse(span, jsonrpc.ID{}, jsonrpc.CodeParseError, "Parse error")
	}
	req, ok := msg.(*jsonrpc.Request)
	if !ok {
		span.SetAttributes(attribute.String(attrMCPMessageType, "not_request"))
		return jsonrpcErrorImmediateResponse(span, jsonrpc.ID{}, jsonrpc.CodeInvalidRequest, "Invalid Request")
	}
	span.SetAttributes(
		semconv.RPCMethod(req.Method),
		attribute.String(attrJSONRPCID, fmt.Sprint(req.ID.Raw())),
		attribute.String(attrJSONRPCMethod, req.Method),
		attribute.String(attrMCPMethod, mcpMethodLabel(req.Method)),
	)
	switch req.Method {
	case methodInitialize:
		return s.handleInitialize(span, req)
	case methodNotificationsInitialized:
		return &mcpProcResponse{Id: req.ID, Immediate: httpStatusResponse(typev3.StatusCode_Accepted)}
	case methodToolsList:
		return jsonrpcImmediateResponse(span, req.ID, &mcp.ListToolsResult{Tools: s.registry.Tools()})
	case methodToolsCall:
		return s.handleToolCall(span, req)
	default:
		return jsonrpcErrorImmediateResponse(span, req.ID, jsonrpc.CodeMethodNotFound, "Method not found")
	}
}

// mcpMethodLabel returns the method itself for supported MCP methods and
// "unknown" otherwise, keeping the span attribute's cardinality bounded.
func mcpMethodLabel(method string) string {
	switch method {
	case methodInitialize, methodNotificationsInitialized, methodToolsList, methodToolsCall:
		return method
	default:
		return "unknown"
	}
}

func (s *extProcServer) handleInitialize(span trace.Span, req *jsonrpc.Request) *mcpProcResponse {
	var initializeParams mcp.InitializeParams
	if err := json.Unmarshal(req.Params, &initializeParams); err != nil {
		zap.L().Debug("Error unmarshalling initialize request", zap.Error(err))
		span.RecordError(err)
		return jsonrpcErrorImmediateResponse(span, req.ID, jsonrpc.CodeInvalidParams, fmt.Sprintf("Invalid params: %v", err))
	}
	negotiatedProtocolVersion := negotiateVersion(initializeParams.ProtocolVersion)
	return jsonrpcImmediateResponse(span, req.ID, &mcp.InitializeResult{
		ProtocolVersion: negotiatedProtocolVersion,
		ServerInfo: &mcp.Implementation{
			Name:    s.serverInfo.Name,
			Version: s.serverInfo.Version,
		},
		Capabilities: &mcp.ServerCapabilities{
			Tools: &mcp.ToolCapabilities{},
		},
		Instructions: s.serverInfo.Instructions,
	})
}

func (s *extProcServer) handleToolCall(span trace.Span, req *jsonrpc.Request) *mcpProcResponse {
	var callParams callToolRequestParams
	if err := json.Unmarshal(req.Params, &callParams); err != nil {
		zap.L().Error("Error unmarshaling tool call request", zap.Error(err))
		span.RecordError(err)
		return jsonrpcErrorImmediateResponse(span, req.ID, jsonrpc.CodeInvalidParams, fmt.Sprintf("Invalid params: %v", err))
	}
	span.SetAttributes(attribute.String(attrToolName, callParams.Name))

	toolConfig := s.registry.GetConfig(callParams.Name)
	if toolConfig == nil {
		return jsonrpcErrorImmediateResponse(span, req.ID, jsonrpc.CodeInvalidParams, fmt.Sprintf("Unknown tool: %s", callParams.Name))
	}

	endpointReq, paramErr := newEndpointRequest(toolConfig.Endpoint, callParams.Arguments)
	if paramErr != nil {
		clientMessage := fmt.Sprintf("Invalid argument %q: %s", paramErr.paramName, paramErr.reason)
		return toolExecutionErrorResponse(span, req.ID, clientMessage, fmt.Sprintf("Invalid parameter in=%s, name=%s: %s", paramErr.paramIn, paramErr.paramName, paramErr.reason))
	}

	path := endpointReq.fullPath()

	requestBody, err := marshalRequestBody(toolConfig.Endpoint, callParams.Arguments)
	if err != nil {
		zap.L().Error("Error marshaling request body", zap.Error(err))
		span.RecordError(err)
		return toolExecutionErrorResponse(span, req.ID, fmt.Sprintf("Failed to serialize 'body' argument: %v", err), "")
	}

	span.SetAttributes(
		semconv.HTTPRequestMethodKey.String(strings.ToUpper(toolConfig.Endpoint.Method)),
		semconv.HTTPRoute(path),
		semconv.HTTPRequestBodySize(len(requestBody)),
	)
	return &mcpProcResponse{
		Id:                 req.ID,
		Reroute:            rerouteWithBodyMutation(toolConfig.Endpoint.Host, strings.ToUpper(toolConfig.Endpoint.Method), path, requestBody, endpointReq.extraHeaders),
		ToolResponseConfig: &toolConfig.toolResponseConfig,
	}
}

func buildMCPResponsePayload(ctx context.Context, jsonrpcID jsonrpc.ID, body []byte, config *toolResponseConfig, errInfo *errorInfo) ([]byte, []*corev3.HeaderValueOption) {
	span := trace.SpanFromContext(ctx)

	buf, err := buildMCPResponse(ctx, jsonrpcID, body, config, errInfo)
	if err != nil {
		span.RecordError(err)
		span.SetAttributes(attribute.String(attrResponseMarshalError, err.Error()))
		span.SetStatus(codes.Error, "failed to build MCP response")
		buf = encodeJSONRPCError(jsonrpcID, jsonrpc.CodeInternalError, "Internal error")
	}

	headers := appendHeader(nil, "content-type", "application/json")
	headers = appendHeader(headers, "content-length", strconv.Itoa(len(buf)))
	return buf, headers
}

// buildEndOfStreamResponse synthesizes an MCP response for a tool call whose
// upstream returned no body (end_of_stream on the response headers).
func buildEndOfStreamResponse(ctx context.Context, requestContext *requestContext) []*extProcPb.ProcessingResponse {
	content := describeEmptyUpstreamResponse(requestContext.upstreamStatusCode)
	errInfo := httpStatusToErrorInfo(requestContext.upstreamStatusCode)

	buf, headers := buildMCPResponsePayload(ctx, requestContext.jsonrpcID, []byte(content), requestContext.toolResponseConfig, errInfo)
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

func (s *extProcServer) mcpResponseHandler(ctx context.Context, body []byte, jsonrpcID jsonrpc.ID, config *toolResponseConfig, errInfo *errorInfo) streamedMutation {
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		attribute.Int(attrBodySize, len(body)),
		attribute.Bool(attrResponseToolCallContextPresent, config != nil),
		attribute.Bool(attrResponseJSONRPCIDPresent, jsonrpcID.IsValid()),
	)
	if jsonrpcID.IsValid() {
		span.SetAttributes(attribute.String(attrJSONRPCID, fmt.Sprint(jsonrpcID.Raw())))
	}
	buf, headers := buildMCPResponsePayload(ctx, jsonrpcID, body, config, errInfo)
	return streamedMutation{
		headers: responseFactory.headerMutation(headers, contentHeaders),
		body:    buf,
	}
}

func buildMCPResponse(ctx context.Context, jsonrpcID jsonrpc.ID, body []byte, config *toolResponseConfig, errorInfo *errorInfo) ([]byte, error) {
	span := trace.SpanFromContext(ctx)
	isError := errorInfo != nil

	if !jsonrpcID.IsValid() {
		return nil, fmt.Errorf("missing req context information: jsonrpcid")
	}

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

	result := &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{
				Text: string(body),
			},
		},
		IsError: isError,
	}
	if structuredContent != nil {
		result.StructuredContent = structuredContent
	}
	resultJson, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	message, err := jsonrpc.EncodeMessage(&jsonrpc.Response{
		ID:     jsonrpcID,
		Result: resultJson,
	})
	if err != nil {
		return nil, err
	}
	return message, nil
}
