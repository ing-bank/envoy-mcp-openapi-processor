package envoy_mcp_openapi_processor

import (
	"net/http"
	"strconv"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
)

// contentHeaders are the headers which are removed whenever the body is replaced.
var contentHeaders = []string{"content-length", "content-type"}

// removeOnRoutingHeaders are the headers dropped when the request is mutated and rerouted
var removeOnRoutingHeaders = []string{"content-length", "content-type", "mcp-protocol-version"}

var (
	// requestFactory provides ext_proc wrappers for request direction
	requestFactory = messageFactory{wrapHeaders: newRequestHeaders, wrapBody: newRequestBody, trailer: requestTrailersResponse}
	// responseFactory provides ext_proc wrappers for response direction
	responseFactory = messageFactory{wrapHeaders: newResponseHeaders, wrapBody: newResponseBody, trailer: responseTrailersResponse}
)

// immediateResponse precedes the immediate arg with an empty request
// headers response: every call site fires while the cycle's RequestHeaders
// message is still unanswered (the state machine defers its response until the
// request body is processed), and the duplex stream needs that in-order ack.
func immediateResponse(immediate *extProcPb.ImmediateResponse) []*extProcPb.ProcessingResponse {
	return []*extProcPb.ProcessingResponse{
		requestFactory.wrapHeaders(&extProcPb.CommonResponse{}),
		{
			Response: &extProcPb.ProcessingResponse_ImmediateResponse{
				ImmediateResponse: immediate,
			},
		},
	}
}

func httpStatusResponse(status typev3.StatusCode) []*extProcPb.ProcessingResponse {
	return immediateResponse(&extProcPb.ImmediateResponse{
		Status: &typev3.HttpStatus{
			Code: status,
		},
	})
}

func methodNotAllowedResponse() []*extProcPb.ProcessingResponse {
	return immediateResponse(&extProcPb.ImmediateResponse{
		Status: &typev3.HttpStatus{
			Code: typev3.StatusCode_MethodNotAllowed,
		},
		Headers: &extProcPb.HeaderMutation{
			SetHeaders: appendHeader(nil, "allow", http.MethodPost),
		},
	})
}

func forbiddenResponse() []*extProcPb.ProcessingResponse {
	return httpStatusResponse(typev3.StatusCode_Forbidden)
}

func appendHeader(headers []*corev3.HeaderValueOption, key string, value string) []*corev3.HeaderValueOption {
	return append(headers, &corev3.HeaderValueOption{
		Header: &corev3.HeaderValue{
			Key:      key,
			RawValue: []byte(value),
		},
		AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
	})
}

func rerouteWithBodyMutation(host string, method string, path string, body []byte, extraHeaders map[string]string) *streamedMutation {
	count := 3 + len(extraHeaders) // (method, path, authority) + extra headers
	headers := make([]*corev3.HeaderValueOption, 0, count)

	for key, value := range extraHeaders {
		headers = appendHeader(headers, key, value)
	}

	headers = appendHeader(headers, ":method", method)
	headers = appendHeader(headers, ":path", path)
	headers = appendHeader(headers, ":authority", host)

	headerMutation := requestFactory.headerMutation(headers, removeOnRoutingHeaders)
	return &streamedMutation{headers: headerMutation, body: body}
}

func newImmediateBodyResponse(body []byte) []*extProcPb.ProcessingResponse {
	return immediateResponse(&extProcPb.ImmediateResponse{
		Body: body,
		Status: &typev3.HttpStatus{
			Code: typev3.StatusCode_OK,
		},
		Headers: &extProcPb.HeaderMutation{
			SetHeaders: appendHeader(nil, "content-type", "application/json"),
		},
	})
}

func newStreamedBodyResponse(chunk []byte, endOfStream bool) *extProcPb.BodyResponse {
	return &extProcPb.BodyResponse{
		Response: &extProcPb.CommonResponse{
			BodyMutation: &extProcPb.BodyMutation{
				Mutation: &extProcPb.BodyMutation_StreamedResponse{
					StreamedResponse: &extProcPb.StreamedBodyResponse{
						Body:        chunk,
						EndOfStream: endOfStream,
					},
				},
			},
		},
	}
}

func newRequestHeaders(response *extProcPb.CommonResponse) *extProcPb.ProcessingResponse {
	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extProcPb.HeadersResponse{Response: response},
		},
	}
}

func newResponseHeaders(response *extProcPb.CommonResponse) *extProcPb.ProcessingResponse {
	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_ResponseHeaders{
			ResponseHeaders: &extProcPb.HeadersResponse{Response: response},
		},
	}
}

func newRequestBody(response *extProcPb.BodyResponse) *extProcPb.ProcessingResponse {
	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_RequestBody{
			RequestBody: response,
		},
	}
}

func newResponseBody(response *extProcPb.BodyResponse) *extProcPb.ProcessingResponse {
	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_ResponseBody{
			ResponseBody: response,
		},
	}
}

func requestTrailersResponse() *extProcPb.ProcessingResponse {
	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_RequestTrailers{
			RequestTrailers: &extProcPb.TrailersResponse{},
		},
	}
}

func responseTrailersResponse() *extProcPb.ProcessingResponse {
	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_ResponseTrailers{
			ResponseTrailers: &extProcPb.TrailersResponse{},
		},
	}
}

// newReplacedResponse builds a response-headers message that replaces the
// upstream response entirely.
func newReplacedResponse(headers []*corev3.HeaderValueOption, body []byte) []*extProcPb.ProcessingResponse {
	headers = appendHeader(headers, ":status", strconv.Itoa(http.StatusOK))
	return []*extProcPb.ProcessingResponse{
		responseFactory.wrapHeaders(&extProcPb.CommonResponse{
			Status: extProcPb.CommonResponse_CONTINUE_AND_REPLACE,
			HeaderMutation: &extProcPb.HeaderMutation{
				SetHeaders:    headers,
				RemoveHeaders: contentHeaders,
			},
			BodyMutation: &extProcPb.BodyMutation{
				Mutation: &extProcPb.BodyMutation_Body{
					Body: body,
				},
			},
		}),
	}
}

// streamedMutation is a header mutation paired with a body to stream to the peer.
// It carries no framing: the transport layer ([frame] function) chunks the body and decides
// end-of-stream and trailers.
type streamedMutation struct {
	headers *extProcPb.ProcessingResponse
	body    []byte
}

// messageFactory bundles the per-direction constructors for the ext_proc
// messages we emit (headers, body, and trailers), so callers can build
// the wire sequence without knowing whether it is framing a request or a response.
type messageFactory struct {
	wrapHeaders func(*extProcPb.CommonResponse) *extProcPb.ProcessingResponse
	wrapBody    func(*extProcPb.BodyResponse) *extProcPb.ProcessingResponse
	trailer     func() *extProcPb.ProcessingResponse
}

// headerMutation builds a headers message that sets and removes the given
// headers in the context of this [messageFactory]
func (f messageFactory) headerMutation(setHeaders []*corev3.HeaderValueOption, removeHeaders []string) *extProcPb.ProcessingResponse {
	return f.wrapHeaders(&extProcPb.CommonResponse{
		HeaderMutation: &extProcPb.HeaderMutation{
			SetHeaders:    setHeaders,
			RemoveHeaders: removeHeaders,
		},
	})
}

// frame turns a [streamedMutation] into the ext_proc wire sequence: the header
// mutation, then the body split into streamed-body chunks no larger than
// streamChunkSize (always at least one, possibly empty, chunk), then (only
// when the source body was terminated by trailers) an empty TrailersResponse.
//
// The final chunk carries end_of_stream only when there are no trailers. If
// trailers are present, they signal the end_of_stream.
func frame(m streamedMutation, factory messageFactory, hasTrailers bool) []*extProcPb.ProcessingResponse {
	reps := []*extProcPb.ProcessingResponse{m.headers}
	body := m.body
	for {
		end := min(len(body), streamChunkSize)
		chunk := body[:end]
		body = body[end:]
		last := len(body) == 0
		reps = append(reps, factory.wrapBody(newStreamedBodyResponse(chunk, last && !hasTrailers)))
		if last {
			break
		}
	}
	if hasTrailers {
		reps = append(reps, factory.trailer())
	}
	return reps
}
