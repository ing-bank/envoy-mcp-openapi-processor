package envoy_mcp_openapi_processor

import (
	"context"
	"fmt"
	"net/http"
	"time"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// streamState represents one phase of the ext_proc protocol state machine.
// The protocol follows the following sequence per request cycle:
//
//	RequestHeaders → RequestBody(chunks) → [RequestTrailers] → ResponseHeaders → ResponseBody(chunks) → [ResponseTrailers] → (next cycle)
//
// Each state handles only the messages valid in its phase, returning the next
// state and the responses to send to Envoy.
type streamState interface {
	handle(ctx context.Context, strm *stream, req *extProcPb.ProcessingRequest) (streamState, []*extProcPb.ProcessingResponse, error)
}

// stream is the per-gRPC-stream context threaded through every state. It holds
// a reference to the server (for its registry and config), the body buffer that
// accumulates chunks across a buffering phase, and the phase start time. It is
// owned by [extProcServer.Process].
type stream struct {
	server *extProcServer
	// buf accumulates the body chunks of one buffering phase; it is reset and
	// reused across phases and cycles.
	buf []byte
	// start marks when the current body-buffering phase began; it back-dates the
	// body-phase span created at finalize, so the span reflects the full
	// buffering duration (including time spent receiving chunks).
	start time.Time
}

// beginBuffering resets the buffer for a new body and records the phase start
// time. No span is opened here: the body-phase span is created at finalize once
// the whole body has arrived, so a stream terminating mid-phase leaks nothing.
func (strm *stream) beginBuffering() {
	strm.buf = strm.buf[:0]
	strm.start = time.Now()
}

type requestContext struct {
	jsonrpcID          jsonrpc.ID
	toolResponseConfig *toolResponseConfig
	upstreamStatusCode int
	// era is set on the request path; read it through
	// [requestContext.protocolEra], which defaults it.
	era protocolEra
}

// protocolEra defaults to legacy, so a context built without one behaves like a
// pre-2026 request.
func (rc *requestContext) protocolEra() protocolEra {
	if rc.era == nil {
		return legacyEra{}
	}
	return rc.era
}

// isToolCall reports whether this cycle's request was a tool call the
// processor rewrote and forwarded upstream and whose response it therefore
// owns and must translate back into an MCP envelope.
func (rc *requestContext) isToolCall() bool {
	return rc.jsonrpcID.IsValid() && rc.toolResponseConfig != nil
}

// stateAwaitingRequestHeaders expects a RequestHeaders message that starts each cycle.
type stateAwaitingRequestHeaders struct{}

func (st *stateAwaitingRequestHeaders) handle(ctx context.Context, strm *stream, req *extProcPb.ProcessingRequest) (streamState, []*extProcPb.ProcessingResponse, error) {
	if hdr, ok := req.Request.(*extProcPb.ProcessingRequest_RequestHeaders); ok {
		_, span := tracer.Start(ctx, "ext_proc.request_headers",
			trace.WithSpanKind(trace.SpanKindInternal),
		)
		defer span.End()
		if method, ok := findHeader(hdr.RequestHeaders.GetHeaders(), ":method"); ok && method != http.MethodPost {
			// MCP delivers all JSON-RPC messages via POST; the Streamable HTTP spec
			// permits 405 for the methods this processor does not support. This is a
			// backstop for deployments whose Envoy config does not filter methods at
			// the route level. The request is not forwarded, so we cycle back to
			// await the next exchange.
			span.SetStatus(codes.Error, "method not allowed")
			return &stateAwaitingRequestHeaders{}, methodNotAllowedResponse(), nil
		}
		if ok, reason := strm.server.allowedHosts.checkHeaders(hdr.RequestHeaders.GetHeaders()); !ok {
			// DNS rebinding protection: refuse requests whose Host/Origin is not
			// allowlisted. With end_of_stream the 403 goes out right away; otherwise
			// body chunks are already in flight on the duplex stream, so the
			// rejection is deferred until the body is drained.
			span.SetStatus(codes.Error, reason)
			zap.L().Warn("blocked request: disallowed host or origin", zap.String("reason", reason))
			if hdr.RequestHeaders.GetEndOfStream() {
				return &stateAwaitingRequestHeaders{}, forbiddenResponse(), nil
			}
			return &stateDrainingRequest{reject: forbiddenResponse()}, nil, nil
		}
		if hdr.RequestHeaders.GetEndOfStream() {
			// An MCP request must carry a JSON-RPC body; end_of_stream on the request
			// headers means no body will follow. Reply with an Invalid Request error
			// instead of stalling in stateBufferingRequest waiting for a body that
			// never arrives. The request is not forwarded, so we cycle back to await
			// the next exchange.
			const msg = "Invalid Request"
			span.SetStatus(codes.Error, msg)
			return &stateAwaitingRequestHeaders{}, newImmediateBodyResponse(encodeJSONRPCError(jsonrpc.ID{}, jsonrpc.CodeInvalidRequest, msg)), nil
		}
		strm.beginBuffering()
		return &stateBufferingRequest{}, nil, nil
	}

	return protocolViolation(req, "RequestHeaders")
}

// stateDrainingRequest discards an in-flight request body whose headers already
// failed validation, then emits the deferred rejection. On the duplex stream
// Envoy has already begun forwarding the request body, so those chunks must be consumed to
// end_of_stream before this state machine can emit the deferred rejection —
// otherwise the in-flight chunks would be treated as an out-of-sequence message
// and fail the stream.
type stateDrainingRequest struct {
	reject []*extProcPb.ProcessingResponse
}

func (st *stateDrainingRequest) handle(ctx context.Context, strm *stream, req *extProcPb.ProcessingRequest) (streamState, []*extProcPb.ProcessingResponse, error) {
	switch value := req.Request.(type) {
	case *extProcPb.ProcessingRequest_RequestBody:
		if !value.RequestBody.GetEndOfStream() {
			return st, nil, nil
		}
		return &stateAwaitingRequestHeaders{}, st.reject, nil
	case *extProcPb.ProcessingRequest_RequestTrailers:
		return &stateAwaitingRequestHeaders{}, st.reject, nil
	default:
		return protocolViolation(req, "RequestBody or RequestTrailers while draining request")
	}
}

// stateBufferingRequest receives RequestBody chunks until EndOfStream.
type stateBufferingRequest struct{}

func (st *stateBufferingRequest) handle(ctx context.Context, strm *stream, req *extProcPb.ProcessingRequest) (streamState, []*extProcPb.ProcessingResponse, error) {
	switch value := req.Request.(type) {
	case *extProcPb.ProcessingRequest_RequestBody:
		strm.buf = append(strm.buf, value.RequestBody.GetBody()...)
		if !value.RequestBody.GetEndOfStream() {
			return st, nil, nil
		}
		return st.finalize(ctx, strm, false)
	case *extProcPb.ProcessingRequest_RequestTrailers:
		// Trailers terminate the request body stream (the final body chunk had
		// end_of_stream=false). Trailer values are not used; we treat the message
		// purely as the end-of-stream marker and process the buffered body.
		return st.finalize(ctx, strm, true)
	default:
		return protocolViolation(req, "RequestBody or RequestTrailers while buffering request")
	}
}

func (st *stateBufferingRequest) finalize(ctx context.Context, strm *stream, hasTrailers bool) (streamState, []*extProcPb.ProcessingResponse, error) {
	ctx, span := tracer.Start(ctx, "ext_proc.request_body",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithTimestamp(strm.start),
	)
	defer span.End()

	handlerResult := strm.server.mcpRequestHandler(ctx, strm.buf)

	reqCtx := requestContext{
		jsonrpcID:          handlerResult.Id,
		toolResponseConfig: handlerResult.ToolResponseConfig,
		era:                handlerResult.Era,
	}

	// A reroute streams the rewritten request body (framed here with trailers if
	// present). An immediate response short-circuits the exchange and is sent
	// verbatim.
	reps := handlerResult.Immediate
	if handlerResult.Reroute != nil {
		reps = frame(*handlerResult.Reroute, requestFactory, hasTrailers)
	}
	return &stateAwaitingResponseHeaders{request: reqCtx}, reps, nil
}

// stateAwaitingResponseHeaders expects the ResponseHeaders message from the upstream.
// If EndOfStream is set (no body), it synthesizes an MCP response and cycles back.
type stateAwaitingResponseHeaders struct {
	request requestContext
}

func (st *stateAwaitingResponseHeaders) handle(ctx context.Context, strm *stream, req *extProcPb.ProcessingRequest) (streamState, []*extProcPb.ProcessingResponse, error) {
	value, ok := req.Request.(*extProcPb.ProcessingRequest_ResponseHeaders)
	if !ok {
		return protocolViolation(req, "ResponseHeaders")
	}

	if !st.request.isToolCall() {
		return protocolViolation(req, "tool call response")
	}

	traceCtx, span := tracer.Start(ctx, "ext_proc.response_headers",
		trace.WithSpanKind(trace.SpanKindInternal),
	)
	defer span.End()

	if statusCode, ok := parseUpstreamStatusCode(value.ResponseHeaders.GetHeaders()); ok {
		st.request.upstreamStatusCode = statusCode
	}

	if !value.ResponseHeaders.GetEndOfStream() {
		strm.beginBuffering()
		return &stateBufferingResponse{request: st.request}, nil, nil
	}

	// Tool call whose upstream returned no body: synthesize an MCP response.
	return &stateAwaitingRequestHeaders{}, buildEndOfStreamResponse(traceCtx, &st.request), nil
}

// stateBufferingResponse receives ResponseBody chunks until EndOfStream is reached.
type stateBufferingResponse struct {
	request requestContext
}

func (st *stateBufferingResponse) handle(ctx context.Context, strm *stream, req *extProcPb.ProcessingRequest) (streamState, []*extProcPb.ProcessingResponse, error) {
	switch value := req.Request.(type) {
	case *extProcPb.ProcessingRequest_ResponseBody:
		strm.buf = append(strm.buf, value.ResponseBody.GetBody()...)
		if !value.ResponseBody.GetEndOfStream() {
			return st, nil, nil
		}
		return st.finalize(ctx, strm, false)
	case *extProcPb.ProcessingRequest_ResponseTrailers:
		// Trailers terminate the response body stream (the final body chunk had
		// end_of_stream=false). Trailer values are not used; we treat the message
		// purely as the end-of-stream marker and process the buffered body.
		return st.finalize(ctx, strm, true)
	default:
		return protocolViolation(req, "ResponseBody or ResponseTrailers while buffering response")
	}
}

func (st *stateBufferingResponse) finalize(ctx context.Context, strm *stream, hasTrailers bool) (streamState, []*extProcPb.ProcessingResponse, error) {
	ctx, span := tracer.Start(ctx, "ext_proc.response_body",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithTimestamp(strm.start),
	)
	defer span.End()

	mutation := mcpResponseHandler(ctx, strm.buf, &st.request)
	reps := frame(mutation, responseFactory, hasTrailers)
	return &stateAwaitingRequestHeaders{}, reps, nil
}

// protocolViolation logs and returns a FailedPrecondition error describing an
// out-of-sequence ext_proc message. The stream is terminated, so the returned
// state and responses are nil (Process ignores them on error).
func protocolViolation(req *extProcPb.ProcessingRequest, expected string) (streamState, []*extProcPb.ProcessingResponse, error) {
	msg := fmt.Sprintf("protocol violation: expected %s, got %T", expected, req.GetRequest())
	zap.L().Error(msg)
	return nil, nil, status.Error(grpccodes.FailedPrecondition, msg)
}
