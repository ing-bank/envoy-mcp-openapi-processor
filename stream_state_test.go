package envoy_mcp_openapi_processor

import (
	"encoding/json"
	"strings"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestProcess_ProtocolViolations(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, "testdata/minimal-products.openapi.yaml")

	requestHeaders := requestHeadersMsg(false)

	toolsCallBody := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestBody{RequestBody: &extProcPb.HttpBody{
			Body:        buildToolsCallRequest(t, "createProduct", map[string]any{"body": map[string]any{"name": "p"}}),
			EndOfStream: true,
		}},
	}
	responseHeaders := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_ResponseHeaders{ResponseHeaders: &extProcPb.HttpHeaders{
			Headers: &corev3.HeaderMap{Headers: []*corev3.HeaderValue{
				{Key: ":status", RawValue: []byte("201")},
			}},
		}},
	}

	tests := []struct {
		name     string
		requests []*extProcPb.ProcessingRequest
		wantMsg  string
	}{
		{
			name: "stateAwaitingRequestHeaders_receives_RequestBody",
			requests: []*extProcPb.ProcessingRequest{
				{Request: &extProcPb.ProcessingRequest_RequestBody{RequestBody: &extProcPb.HttpBody{}}},
			},
			wantMsg: "protocol violation: expected RequestHeaders, got *ext_procv3.ProcessingRequest_RequestBody",
		},
		{
			name:     "stateBufferingRequest_receives_RequestHeaders",
			requests: []*extProcPb.ProcessingRequest{requestHeaders, requestHeaders},
			wantMsg:  "protocol violation: expected RequestBody or RequestTrailers while buffering request, got *ext_procv3.ProcessingRequest_RequestHeaders",
		},
		{
			name: "stateAwaitingResponseHeaders_receives_ResponseBody",
			requests: []*extProcPb.ProcessingRequest{
				requestHeaders,
				{Request: &extProcPb.ProcessingRequest_RequestBody{RequestBody: &extProcPb.HttpBody{EndOfStream: true}}},
				{Request: &extProcPb.ProcessingRequest_ResponseBody{ResponseBody: &extProcPb.HttpBody{}}},
			},
			wantMsg: "protocol violation: expected ResponseHeaders, got *ext_procv3.ProcessingRequest_ResponseBody",
		},
		{
			// The processor answered tools/list itself; an upstream response
			// arriving for a request it never forwarded is out of sequence.
			name: "stateAwaitingResponseHeaders_receives_ResponseHeaders_for_non_tool_call",
			requests: []*extProcPb.ProcessingRequest{
				requestHeaders,
				{Request: &extProcPb.ProcessingRequest_RequestBody{RequestBody: &extProcPb.HttpBody{
					Body:        []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
					EndOfStream: true,
				}}},
				responseHeaders,
			},
			wantMsg: "protocol violation: expected tool call response, got *ext_procv3.ProcessingRequest_ResponseHeaders",
		},
		{
			name: "stateBufferingResponse_receives_ResponseHeaders",
			requests: []*extProcPb.ProcessingRequest{
				requestHeaders,
				toolsCallBody,
				responseHeaders,
				{Request: &extProcPb.ProcessingRequest_ResponseHeaders{ResponseHeaders: &extProcPb.HttpHeaders{}}},
			},
			wantMsg: "protocol violation: expected ResponseBody or ResponseTrailers while buffering response, got *ext_procv3.ProcessingRequest_ResponseHeaders",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stream := &fakeProcessStream{requests: tt.requests}
			err := server.Process(stream)
			require.Error(t, err)

			st, ok := status.FromError(err)
			require.True(t, ok)
			assert.Equal(t, grpccodes.FailedPrecondition, st.Code())
			assert.Contains(t, st.Message(), tt.wantMsg)
		})
	}
}

func TestProcess_TrailersTerminateBody(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, "testdata/minimal-products.openapi.yaml")

	// Trailers present: the final body chunk carries end_of_stream=false and the
	// trailers message terminates the body stream. Both the request and response
	// bodies are terminated this way; the processor must still emit its mutations.
	requestBody := buildToolsCallRequest(t, "createProduct", map[string]any{
		"body": map[string]any{"name": "trailer-pet"},
	})
	upstreamResponse := []byte(`{"id":7,"name":"trailer-pet"}`)

	requests := []*extProcPb.ProcessingRequest{
		requestHeadersMsg(false),
		{Request: &extProcPb.ProcessingRequest_RequestBody{RequestBody: &extProcPb.HttpBody{Body: requestBody, EndOfStream: false}}},
		{Request: &extProcPb.ProcessingRequest_RequestTrailers{RequestTrailers: &extProcPb.HttpTrailers{}}},
		{Request: &extProcPb.ProcessingRequest_ResponseHeaders{ResponseHeaders: &extProcPb.HttpHeaders{
			Headers: &corev3.HeaderMap{Headers: []*corev3.HeaderValue{
				{Key: ":status", RawValue: []byte("201")},
			}},
		}}},
		{Request: &extProcPb.ProcessingRequest_ResponseBody{ResponseBody: &extProcPb.HttpBody{Body: upstreamResponse, EndOfStream: false}}},
		{Request: &extProcPb.ProcessingRequest_ResponseTrailers{ResponseTrailers: &extProcPb.HttpTrailers{}}},
	}

	stream := &fakeProcessStream{requests: requests}
	require.NoError(t, server.Process(stream))

	// The buffered request body was rerouted (mutation emitted) even though the
	// body chunk never set end_of_stream, and the response was translated. Each
	// direction must be followed by a TrailersResponse acknowledging the trailers.
	// The chunk-level framing invariants (end_of_stream=false on every chunk,
	// empty TrailersResponse) are pinned by TestFrame.
	var (
		sawRequestBody, sawResponseBody         bool
		sawRequestTrailers, sawResponseTrailers bool
	)
	for _, resp := range stream.sentResponses {
		sawRequestBody = sawRequestBody || resp.GetRequestBody() != nil
		sawResponseBody = sawResponseBody || resp.GetResponseBody() != nil
		sawRequestTrailers = sawRequestTrailers || resp.GetRequestTrailers() != nil
		sawResponseTrailers = sawResponseTrailers || resp.GetResponseTrailers() != nil
	}
	assert.True(t, sawRequestBody, "request trailers should finalize and emit the rerouted request body")
	assert.True(t, sawResponseBody, "response trailers should finalize and emit the translated response body")
	assert.True(t, sawRequestTrailers, "request trailers should be acknowledged with an empty TrailersResponse")
	assert.True(t, sawResponseTrailers, "response trailers should be acknowledged with an empty TrailersResponse")
}

func TestProcess_ResponseHeadersEOS(t *testing.T) {
	t.Parallel()

	// ResponseHeaders with end_of_stream set means the upstream sent no body:
	// the state machine routes to buildEndOfStreamResponse, which synthesizes
	// the MCP response in the headers message itself (CONTINUE_AND_REPLACE).
	// This pins the state-machine branch; the status -> isError mapping and the
	// per-status synthesized text are unit-tested directly in mcp_handler tests
	// (TestDescribeEmptyUpstreamResponse, TestHTTPStatusToErrorInfo,
	// TestPetstoreProtocolTranslation_ResponseBodyMarksAPIFailuresAsToolErrors).
	server := newTestServer(t, "testdata/petstore.openapi.yaml")

	requestBody := buildToolsCallRequest(t, "getPetById", map[string]any{"petId": 123})
	requests := []*extProcPb.ProcessingRequest{
		requestHeadersMsg(false),
		{Request: &extProcPb.ProcessingRequest_RequestBody{
			RequestBody: &extProcPb.HttpBody{Body: requestBody, EndOfStream: true},
		}},
		{Request: &extProcPb.ProcessingRequest_ResponseHeaders{
			ResponseHeaders: &extProcPb.HttpHeaders{
				EndOfStream: true,
				Headers: &corev3.HeaderMap{Headers: []*corev3.HeaderValue{
					{Key: ":status", RawValue: []byte("204")},
				}},
			},
		}},
	}
	stream := &fakeProcessStream{requests: requests}
	require.NoError(t, server.Process(stream))
	require.Len(t, stream.sentResponses, 3)

	eosResponse := stream.sentResponses[2].GetResponseHeaders()
	require.NotNil(t, eosResponse)
	common := eosResponse.GetResponse()
	require.NotNil(t, common)

	assert.Equal(t, extProcPb.CommonResponse_CONTINUE_AND_REPLACE, common.GetStatus())
	gotHeaders := headersFromSetHeaders(common.GetHeaderMutation().GetSetHeaders())
	assert.Equal(t, "200", gotHeaders[":status"])
	assert.Equal(t, "application/json", gotHeaders["content-type"])

	jsonrpcResponse := decodeJSONRPCResponseBody(t, common.GetBodyMutation().GetBody())
	result, ok := jsonrpcResponse["result"].(map[string]any)
	require.True(t, ok)

	content, ok := result["content"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, content)
	firstContent, ok := content[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "API returned HTTP 204 (No Content) with no response body.", firstContent["text"])
}

func TestProcess_BodySpanOnlyOnCompletedPhase(t *testing.T) {
	spanRecorder := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRecorder)))

	server := &extProcServer{allowedHosts: newHostAllowlist(nil)}

	// Abnormal termination mid-phase: drive the stream into stateBufferingRequest
	// with a non-final chunk, then violate the protocol so Process exits before
	// end-of-stream. The body span is created only at finalize, so no
	// ext_proc.request_body span exists - nothing was started, nothing leaked.
	abnormal := &fakeProcessStream{requests: []*extProcPb.ProcessingRequest{
		requestHeadersMsg(false),
		{Request: &extProcPb.ProcessingRequest_RequestBody{RequestBody: &extProcPb.HttpBody{Body: []byte("{"), EndOfStream: false}}},
		requestHeadersMsg(false),
	}}
	require.Error(t, server.Process(abnormal))

	_, found := findSpanByName(spanRecorder.Ended(), "ext_proc.request_body")
	assert.False(t, found, "no request body span until the body phase completes")

	// Completed phase: a full request body (end_of_stream=true) reaches finalize,
	// which creates and ends exactly one ext_proc.request_body span. The body is
	// intentionally invalid JSON so the handler short-circuits without touching the
	// (nil) registry; the span is emitted regardless of the handler's verdict.
	completed := &fakeProcessStream{requests: []*extProcPb.ProcessingRequest{
		requestHeadersMsg(false),
		{Request: &extProcPb.ProcessingRequest_RequestBody{RequestBody: &extProcPb.HttpBody{Body: []byte("{"), EndOfStream: true}}},
	}}
	require.NoError(t, server.Process(completed))

	span, found := findSpanByName(spanRecorder.Ended(), "ext_proc.request_body")
	require.True(t, found, "completed body phase must emit an ended request body span")
	assert.False(t, span.StartTime().After(span.EndTime()),
		"span is back-dated to the phase start, so it must not end before it starts")
}

func TestProcess_BodylessRequest(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, "testdata/minimal-products.openapi.yaml")

	requests := []*extProcPb.ProcessingRequest{
		requestHeadersMsg(true),
	}
	stream := &fakeProcessStream{requests: requests}
	require.NoError(t, server.Process(stream))

	immediate := requireImmediateResponse(t, stream.sentResponses)
	assert.Equal(t, int32(200), int32(immediate.GetStatus().GetCode()),
		"protocol errors should use HTTP 200 with a JSON-RPC error body")
	assertJSONRPCErrorBody(t, immediate.GetBody(), jsonrpc.CodeInvalidRequest)

	for _, resp := range stream.sentResponses {
		assert.Nil(t, resp.GetRequestBody(), "bodyless request must not be forwarded upstream")
	}
}

func TestProcess_NonPOSTRequest(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, "testdata/minimal-products.openapi.yaml")

	// MCP delivers all JSON-RPC messages via POST. A request whose :method is
	// anything else must be rejected with HTTP 405. The method guard fires
	// before the end_of_stream check and does not branch on the specific
	// method, so one non-POST verb exercises the whole path; cross-method
	// coverage at the Envoy layer is the e2e suite's job (GET/DELETE -> 405).
	requests := []*extProcPb.ProcessingRequest{
		{
			Request: &extProcPb.ProcessingRequest_RequestHeaders{
				RequestHeaders: &extProcPb.HttpHeaders{
					Headers: &corev3.HeaderMap{Headers: []*corev3.HeaderValue{
						{Key: ":method", RawValue: []byte("GET")},
					}},
				},
			},
		},
	}
	stream := &fakeProcessStream{requests: requests}
	require.NoError(t, server.Process(stream))

	immediate := requireImmediateResponse(t, stream.sentResponses)
	assert.Equal(t, int32(405), int32(immediate.GetStatus().GetCode()))
	gotHeaders := headersFromSetHeaders(immediate.GetHeaders().GetSetHeaders())
	assert.Equal(t, "POST", gotHeaders["allow"])

	for _, resp := range stream.sentResponses {
		assert.Nil(t, resp.GetRequestBody(), "non-POST request must not be forwarded upstream")
	}
}

func TestProcess_DisallowedHost_NoBody(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, "testdata/minimal-products.openapi.yaml")

	// DNS rebinding protection: a bodyless request with a non-localhost Host is
	// refused with HTTP 403 right at the headers phase.
	requests := []*extProcPb.ProcessingRequest{
		{
			Request: &extProcPb.ProcessingRequest_RequestHeaders{
				RequestHeaders: &extProcPb.HttpHeaders{
					EndOfStream: true,
					Headers: &corev3.HeaderMap{Headers: []*corev3.HeaderValue{
						{Key: ":authority", RawValue: []byte("evil.com")},
					}},
				},
			},
		},
	}
	stream := &fakeProcessStream{requests: requests}
	require.NoError(t, server.Process(stream))

	immediate := requireImmediateResponse(t, stream.sentResponses)
	assert.Equal(t, int32(403), int32(immediate.GetStatus().GetCode()))
	for _, resp := range stream.sentResponses {
		assert.Nil(t, resp.GetRequestBody(), "blocked request must not be forwarded upstream")
	}
}

func TestProcess_DisallowedHost_DrainsBody(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, "testdata/minimal-products.openapi.yaml")

	// When the disallowed request carries a body, its chunks are already in
	// flight on the duplex stream: they must be drained without protocol
	// violations and the 403 sent only once the body ends — via end_of_stream
	// or via trailers.
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	tests := []struct {
		name string
		tail []*extProcPb.ProcessingRequest
	}{
		{
			name: "terminated_by_end_of_stream",
			tail: []*extProcPb.ProcessingRequest{
				{Request: &extProcPb.ProcessingRequest_RequestBody{RequestBody: &extProcPb.HttpBody{Body: body[:10], EndOfStream: false}}},
				{Request: &extProcPb.ProcessingRequest_RequestBody{RequestBody: &extProcPb.HttpBody{Body: body[10:], EndOfStream: true}}},
			},
		},
		{
			name: "terminated_by_trailers",
			tail: []*extProcPb.ProcessingRequest{
				{Request: &extProcPb.ProcessingRequest_RequestBody{RequestBody: &extProcPb.HttpBody{Body: body, EndOfStream: false}}},
				{Request: &extProcPb.ProcessingRequest_RequestTrailers{RequestTrailers: &extProcPb.HttpTrailers{}}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requests := []*extProcPb.ProcessingRequest{
				{
					Request: &extProcPb.ProcessingRequest_RequestHeaders{
						RequestHeaders: &extProcPb.HttpHeaders{
							Headers: &corev3.HeaderMap{Headers: []*corev3.HeaderValue{
								{Key: ":authority", RawValue: []byte("evil.com")},
							}},
						},
					},
				},
			}
			requests = append(requests, tt.tail...)
			stream := &fakeProcessStream{requests: requests}
			require.NoError(t, server.Process(stream), "draining a blocked request must not be a protocol violation")

			// The rejection is deferred: nothing goes out until the body has been
			// drained, then the headers ack and the immediate 403 follow.
			immediate := requireImmediateResponse(t, stream.sentResponses)
			assert.Equal(t, int32(403), int32(immediate.GetStatus().GetCode()))
			require.Len(t, stream.sentResponses, 2)
			assert.NotNil(t, stream.sentResponses[0].GetRequestHeaders(), "the deferred rejection must still ack the request headers first")
			for _, resp := range stream.sentResponses {
				assert.Nil(t, resp.GetRequestBody(), "blocked request must not be forwarded upstream")
			}
		})
	}
}

func TestProcess_AllowedHostAndOrigin(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, "testdata/minimal-products.openapi.yaml")

	// A localhost Host plus a localhost Origin (MCP inspector style) passes
	// validation and the request is processed normally.
	requests := []*extProcPb.ProcessingRequest{
		requestHeadersMsg(false, &corev3.HeaderValue{Key: "origin", RawValue: []byte("http://localhost:6274")}),
		{Request: &extProcPb.ProcessingRequest_RequestBody{RequestBody: &extProcPb.HttpBody{
			Body:        []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
			EndOfStream: true,
		}}},
	}
	stream := &fakeProcessStream{requests: requests}
	require.NoError(t, server.Process(stream))

	immediate := requireImmediateResponse(t, stream.sentResponses)
	assert.Equal(t, int32(200), int32(immediate.GetStatus().GetCode()))
	assert.Contains(t, string(immediate.GetBody()), `"tools"`)
}

func TestProcess_BodyChunking(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, "testdata/minimal-products.openapi.yaml")

	// incomingChunkSize differs from streamChunkSize so input framing cannot
	// accidentally line up with output framing.
	const incomingChunkSize = 16 * 1024

	splitBody := func(data []byte, wrap func(chunk []byte, eos bool) *extProcPb.ProcessingRequest) []*extProcPb.ProcessingRequest {
		var msgs []*extProcPb.ProcessingRequest
		for len(data) > 0 {
			chunk := data[:min(len(data), incomingChunkSize)]
			data = data[len(chunk):]
			msgs = append(msgs, wrap(chunk, len(data) == 0))
		}
		return msgs
	}

	bodyArg := map[string]any{"data": strings.Repeat("x", 70*1024)}
	upstreamResponse := []byte(`{"id":1,"data":"` + strings.Repeat("x", 70*1024) + `"}`)

	var requests []*extProcPb.ProcessingRequest
	requests = append(requests, requestHeadersMsg(false))
	requests = append(requests, splitBody(buildToolsCallRequest(t, "createProduct", map[string]any{"body": bodyArg}), func(chunk []byte, eos bool) *extProcPb.ProcessingRequest {
		return &extProcPb.ProcessingRequest{
			Request: &extProcPb.ProcessingRequest_RequestBody{RequestBody: &extProcPb.HttpBody{Body: chunk, EndOfStream: eos}},
		}
	})...)
	requests = append(requests, &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_ResponseHeaders{ResponseHeaders: &extProcPb.HttpHeaders{
			Headers: &corev3.HeaderMap{Headers: []*corev3.HeaderValue{
				{Key: ":status", RawValue: []byte("201")},
			}},
		}},
	})
	requests = append(requests, splitBody(upstreamResponse, func(chunk []byte, eos bool) *extProcPb.ProcessingRequest {
		return &extProcPb.ProcessingRequest{
			Request: &extProcPb.ProcessingRequest_ResponseBody{ResponseBody: &extProcPb.HttpBody{Body: chunk, EndOfStream: eos}},
		}
	})...)

	stream := &fakeProcessStream{requests: requests}
	require.NoError(t, server.Process(stream))

	// Reassembled rerouted request body matches the JSON-encoded tool argument,
	// emitted as more than one streamed chunk.
	var requestChunks int
	var requestBody []byte
	for _, resp := range stream.sentResponses {
		if rb := resp.GetRequestBody(); rb != nil {
			streamed := rb.GetResponse().GetBodyMutation().GetStreamedResponse()
			require.NotNil(t, streamed, "rerouted request body must be streamed")
			requestChunks++
			requestBody = append(requestBody, streamed.GetBody()...)
		}
	}
	assert.Greater(t, requestChunks, 1, "large request body should be split into multiple chunks")
	expected, err := json.Marshal(bodyArg)
	require.NoError(t, err)
	assert.JSONEq(t, string(expected), string(requestBody))

	// Reassembled response body is a valid JSON-RPC result carrying the
	// upstream payload.
	assertResponseTranslation(t, stream.sentResponses, upstreamResponse, responseExpectation{
		ExpectContentTextEqUpstream: true,
		IsStructured:                false,
		IsError:                     false,
	})
}
