package envoy_mcp_openapi_processor

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"
)

// mustMakeID creates a jsonrpc.ID from the given value or panics.
func mustMakeID(v any) jsonrpc.ID {
	id, err := jsonrpc.MakeID(v)
	if err != nil {
		panic(err)
	}
	return id
}

func requestHeadersMsg(endOfStream bool, extra ...*corev3.HeaderValue) *extProcPb.ProcessingRequest {
	headers := append([]*corev3.HeaderValue{
		{Key: ":authority", RawValue: []byte("localhost")},
	}, extra...)
	return &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extProcPb.HttpHeaders{
				EndOfStream: endOfStream,
				Headers:     &corev3.HeaderMap{Headers: headers},
			},
		},
	}
}

// newTestServer creates a properly configured extProcServer for testing.
func newTestServer(t *testing.T, openAPIPath string) *extProcServer {
	t.Helper()
	registry, err := newToolRegistryFromConfig(&ToolRegistryConfig{OpenAPISpecPattern: openAPIPath})
	if err != nil {
		t.Fatalf("Warning: failed to load tools config: %v", err)
	}
	return &extProcServer{registry: registry, allowedHosts: newHostAllowlist(nil)}
}

// findResponse returns the first response in the sequence matching pred, or
// fails the test naming what was expected.
func findResponse(t *testing.T, reps []*extProcPb.ProcessingResponse, desc string, pred func(*extProcPb.ProcessingResponse) bool) *extProcPb.ProcessingResponse {
	t.Helper()
	for _, resp := range reps {
		if pred(resp) {
			return resp
		}
	}
	t.Fatalf("expected %s in processing response sequence", desc)
	return nil
}

func requireResponseBodyResponse(t *testing.T, reps []*extProcPb.ProcessingResponse) *extProcPb.ProcessingResponse {
	t.Helper()
	return findResponse(t, reps, "response body response", func(r *extProcPb.ProcessingResponse) bool { return r.GetResponseBody() != nil })
}

func requireResponseHeadersResponse(t *testing.T, reps []*extProcPb.ProcessingResponse) *extProcPb.ProcessingResponse {
	t.Helper()
	return findResponse(t, reps, "response headers response", func(r *extProcPb.ProcessingResponse) bool { return r.GetResponseHeaders() != nil })
}

func requireImmediateResponse(t *testing.T, reps []*extProcPb.ProcessingResponse) *extProcPb.ImmediateResponse {
	t.Helper()
	return findResponse(t, reps, "immediate response", func(r *extProcPb.ProcessingResponse) bool { return r.GetImmediateResponse() != nil }).GetImmediateResponse()
}

func decodeJSONRPCResult(t *testing.T, body []byte) (envelope map[string]any, result map[string]any) {
	t.Helper()
	require.NoError(t, json.Unmarshal(body, &envelope), "response body should be valid JSON")
	result, ok := envelope["result"].(map[string]any)
	require.True(t, ok, "response should contain a 'result' object, got: %s", body)
	return envelope, result
}

func assertServerInfoMeta(t *testing.T, result map[string]any, wantName, wantVersion string) {
	t.Helper()
	meta, ok := result["_meta"].(map[string]any)
	require.True(t, ok, "modern result should carry _meta")
	serverInfo, ok := meta[mcp.MetaKeyServerInfo].(map[string]any)
	require.True(t, ok, "_meta should carry %s", mcp.MetaKeyServerInfo)
	assert.Equal(t, wantName, serverInfo["name"])
	assert.Equal(t, wantVersion, serverInfo["version"])
}

func collectResponseBodyBytes(t *testing.T, reps []*extProcPb.ProcessingResponse) []byte {
	t.Helper()
	var body []byte
	found := false
	for _, resp := range reps {
		responseBody := resp.GetResponseBody()
		if responseBody == nil {
			continue
		}
		found = true
		streamed := responseBody.GetResponse().GetBodyMutation().GetStreamedResponse()
		if streamed == nil {
			t.Fatal("expected streamed response body mutation")
		}
		body = append(body, streamed.GetBody()...)
	}
	if !found {
		t.Fatal("expected response body response in processing response sequence")
	}
	return body
}

type fakeProcessStream struct {
	extProcPb.ExternalProcessor_ProcessServer
	ctx           context.Context
	requests      []*extProcPb.ProcessingRequest
	recvErr       error // returned once requests are exhausted; defaults to io.EOF
	sentResponses []*extProcPb.ProcessingResponse
	readIndex     int
}

func (s *fakeProcessStream) Recv() (*extProcPb.ProcessingRequest, error) {
	if s.readIndex >= len(s.requests) {
		if s.recvErr != nil {
			return nil, s.recvErr
		}
		return nil, io.EOF
	}
	request := s.requests[s.readIndex]
	s.readIndex++
	return request, nil
}

func (s *fakeProcessStream) Send(resp *extProcPb.ProcessingResponse) error {
	s.sentResponses = append(s.sentResponses, resp)
	return nil
}

func (s *fakeProcessStream) Context() context.Context {
	if s.ctx == nil {
		return context.Background()
	}
	return s.ctx
}

func (s *fakeProcessStream) SetHeader(metadata.MD) error { return nil }

func (s *fakeProcessStream) SendHeader(metadata.MD) error { return nil }

func (s *fakeProcessStream) SetTrailer(metadata.MD) {}

func (s *fakeProcessStream) SendMsg(any) error { return nil }

func (s *fakeProcessStream) RecvMsg(any) error { return nil }
