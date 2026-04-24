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
	Id            jsonrpc.ID
	ProcRep       *extProcPb.ProcessingResponse
	ToolConfigRef *toolConfig
}

func isHTTPErrorStatus(statusCode int) bool {
	return statusCode == 0 || statusCode >= 400
}

func parseUpstreamStatusCode(headers *corev3.HeaderMap) (int, bool) {
	if headers == nil {
		return 0, false
	}
	for _, h := range headers.GetHeaders() {
		if h.GetKey() != ":status" {
			continue
		}
		statusCode, err := strconv.Atoi(string(h.GetRawValue()))
		if err != nil {
			return 0, false
		}
		return statusCode, true
	}
	return 0, false
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
	return &mcpProcResponse{Id: id, ProcRep: newImmediateBodyResponse(encodeJSONRPCError(id, code, message))}
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
	return &mcpProcResponse{Id: id, ProcRep: newImmediateBodyResponse(message)}
}

func (s *extProcServer) mcpRequestHandler(ctx context.Context, body []byte) *mcpProcResponse {
	span := trace.SpanFromContext(ctx)

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
		attribute.String(attrJSONRPCID, fmt.Sprint(req.ID)),
		attribute.String(attrJSONRPCMethod, req.Method),
	)
	switch req.Method {
	case methodInitialize:
		span.SetAttributes(attribute.String(attrMCPMethod, methodInitialize))
		return s.handleInitialize(span, req)
	case methodNotificationsInitialized:
		span.SetAttributes(attribute.String(attrMCPMethod, methodNotificationsInitialized))
		return &mcpProcResponse{req.ID, httpStatusResponse(typev3.StatusCode_Accepted), nil}
	case methodToolsList:
		span.SetAttributes(attribute.String(attrMCPMethod, methodToolsList))
		return jsonrpcImmediateResponse(span, req.ID, &mcp.ListToolsResult{Tools: s.registry.Tools()})
	case methodToolsCall:
		span.SetAttributes(attribute.String(attrMCPMethod, methodToolsCall))
		return s.handleToolCall(span, req)
	default:
		span.SetAttributes(attribute.String(attrMCPMethod, "unknown"))
		return jsonrpcErrorImmediateResponse(span, req.ID, jsonrpc.CodeMethodNotFound, "Method not found")
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
	return &mcpProcResponse{req.ID, rerouteWithBodyMutation(toolConfig.Endpoint.Host, strings.ToUpper(toolConfig.Endpoint.Method), path, requestBody, endpointReq.extraHeaders), toolConfig}
}

func buildEndOfStreamResponse(ctx context.Context, jsonrpcID jsonrpc.ID, upstreamStatusCode int, toolRepConfig *toolResponseConfig) *extProcPb.ProcessingResponse {
	content := describeEmptyUpstreamResponse(upstreamStatusCode)
	mcpRep := buildMCPCommonResponse(ctx, jsonrpcID, []byte(content), toolRepConfig, httpStatusToErrorInfo(upstreamStatusCode))
	mcpRep.Status = extProcPb.CommonResponse_CONTINUE_AND_REPLACE
	mcpRep.HeaderMutation.SetHeaders = appendHeader(
		mcpRep.HeaderMutation.SetHeaders, ":status", strconv.Itoa(http.StatusOK),
	)
	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_ResponseHeaders{
			ResponseHeaders: &extProcPb.HeadersResponse{Response: mcpRep},
		},
	}
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

func (s *extProcServer) mcpResponseHandler(ctx context.Context, body []byte, jsonrpcID jsonrpc.ID, config *toolResponseConfig, error *errorInfo) *extProcPb.ProcessingResponse {
	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_ResponseBody{
			ResponseBody: &extProcPb.BodyResponse{
				Response: buildMCPCommonResponse(ctx, jsonrpcID, body, config, error),
			},
		},
	}
}

func buildMCPCommonResponse(ctx context.Context, jsonrpcID jsonrpc.ID, body []byte, config *toolResponseConfig, error *errorInfo) *extProcPb.CommonResponse {
	span := trace.SpanFromContext(ctx)

	buf, err := buildMCPResponse(ctx, jsonrpcID, body, config, error)
	if err != nil {
		span.RecordError(err)
		span.SetAttributes(attribute.String(attrResponseMarshalError, err.Error()))
		span.SetStatus(codes.Error, "failed to build MCP response")
		buf = encodeJSONRPCError(jsonrpcID, jsonrpc.CodeInternalError, "Internal error")
	}

	return &extProcPb.CommonResponse{
		BodyMutation: &extProcPb.BodyMutation{
			Mutation: &extProcPb.BodyMutation_Body{Body: buf},
		},
		HeaderMutation: &extProcPb.HeaderMutation{
			SetHeaders: []*corev3.HeaderValueOption{
				{
					Header: &corev3.HeaderValue{
						Key:      "content-type",
						RawValue: []byte("application/json"),
					},
					AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
				},
				{
					Header: &corev3.HeaderValue{
						Key:      "content-length",
						RawValue: []byte(strconv.Itoa(len(buf))),
					},
					AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
				},
			},
		},
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
