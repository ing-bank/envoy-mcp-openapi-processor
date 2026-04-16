package envoy_mcp_openapi_processor

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
)

func TestMcpRequestHandler_Tracing(t *testing.T) {
	tests := []struct {
		name                  string
		requestBody           string
		wantNilResp           bool
		wantMethod            string
		wantMCPMethod         string
		wantStatusCode        codes.Code
		wantStatusDescription string
		wantErrorEvent        bool
		wantExceptionType     string
		wantToolErrorReason   string
	}{
		// immediate response methods
		{
			name:           "initialize",
			requestBody:    `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
			wantNilResp:    false,
			wantMethod:     "initialize",
			wantMCPMethod:  "initialize",
			wantStatusCode: codes.Unset,
			wantErrorEvent: false,
		},
		{
			name:           "tools/list",
			requestBody:    `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
			wantNilResp:    false,
			wantMethod:     "tools/list",
			wantMCPMethod:  "tools/list",
			wantStatusCode: codes.Unset,
			wantErrorEvent: false,
		},
		// json-rpc errors
		{
			name:                  "parse error invalid json",
			requestBody:           `{invalid json`,
			wantStatusCode:        codes.Error,
			wantStatusDescription: "Parse error",
			wantErrorEvent:        true,
			wantExceptionType:     "*fmt.wrapError",
		},
		{
			name:                  "invalid request response object",
			requestBody:           `{"jsonrpc":"2.0","id":1,"result":"x"}`,
			wantStatusCode:        codes.Error,
			wantStatusDescription: "Invalid Request",
		},
		{
			name:                  "method not found",
			requestBody:           `{"jsonrpc":"2.0","id":1,"method":"prompts/unknown","params":{}}`,
			wantStatusCode:        codes.Error,
			wantStatusDescription: "Method not found",
		},
		{
			name:                  "invalid params initialize",
			requestBody:           `{"jsonrpc":"2.0","id":1,"method":"initialize","params":"bad"}`,
			wantMethod:            "initialize",
			wantMCPMethod:         "initialize",
			wantStatusCode:        codes.Error,
			wantStatusDescription: "Invalid params: json",
			wantErrorEvent:        true,
			wantExceptionType:     "*json.UnmarshalTypeError",
		},
		{
			name:                  "invalid params tools call",
			requestBody:           `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"bad"}`,
			wantMethod:            "tools/call",
			wantMCPMethod:         "tools/call",
			wantStatusCode:        codes.Error,
			wantStatusDescription: "Invalid params: json",
			wantErrorEvent:        true,
			wantExceptionType:     "*json.UnmarshalTypeError",
		},
		// tool execution errors
		{
			name:                "missing required path parameter",
			requestBody:         `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"getPetById","arguments":{}}}`,
			wantToolErrorReason: "Invalid parameter in=path, name=petId: required but missing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracer, spanRecorder := setupTestTracer()
			server := newTestServer(t, "testdata/petstore.openapi.yaml")
			spanName := "test_" + tt.name
			ctx, rootSpan := tracer.Start(context.Background(), spanName)

			// Call the handler
			resp := server.mcpRequestHandler(ctx, []byte(tt.requestBody))
			assert.True(t, tt.wantNilResp == (resp == nil), "mcpRequestHandler() nil return")

			rootSpan.End()

			spans := spanRecorder.Ended()
			assert.NotEmpty(t, spans, "No spans recorded")

			testSpan, found := findSpanByName(spans, spanName)
			assert.True(t, found, "span '%s' not found", spanName)

			// Verify attributes if method is expected
			if tt.wantMethod != "" {
				got := getSpanAttribute(testSpan, string(semconv.RPCMethodKey))
				assert.Equal(t, tt.wantMethod, got, "rpc.method")

				// Check for jsonrpc.id presence (except for invalid JSON)
				if !tt.wantNilResp {
					got := getSpanAttribute(testSpan, attrJSONRPCID)
					assert.NotEmpty(t, got, "jsonrpc.id attribute")
				}
			}

			if tt.wantMCPMethod != "" {
				got := getSpanAttribute(testSpan, attrMCPMethod)
				assert.Equal(t, tt.wantMCPMethod, got, "mcp.method")
			}

			assertToolError(t, testSpan, tt.wantToolErrorReason)

			// Verify span status
			assert.Equal(t, tt.wantStatusCode, testSpan.Status().Code, "span status")
			assert.Contains(t, testSpan.Status().Description, tt.wantStatusDescription, "span status description")

			// Verify error events match expectations
			assertExceptionEvents(t, testSpan, tt.wantErrorEvent, tt.wantExceptionType)
		})
	}
}

func TestMcpResponseHandler_Tracing(t *testing.T) {
	tests := []struct {
		name                string
		responseBody        string
		jsonrpcID           jsonrpc.ID
		IsError             bool
		wantStatusCode      codes.Code
		wantErrorEvent      bool
		wantExceptionType   string
		wantToolErrorReason string
	}{
		{
			name:              "valid response",
			responseBody:      `{"result": "success", "data": {"key": "value"}}`,
			jsonrpcID:         mustMakeID("test-id-001"),
			wantStatusCode:    codes.Unset,
			wantErrorEvent:    false,
			wantExceptionType: "",
		},
		{
			// For non-json responses, we return a non-structured response to mcp client
			// so we are not raising an error here
			name:              "invalid JSON response",
			responseBody:      `{invalid json}`,
			jsonrpcID:         mustMakeID("test-id-002"),
			wantStatusCode:    codes.Unset,
			wantErrorEvent:    false,
			wantExceptionType: "",
		},
		{
			name:              "empty response",
			responseBody:      `{}`,
			jsonrpcID:         mustMakeID("test-id-003"),
			wantStatusCode:    codes.Unset,
			wantErrorEvent:    false,
			wantExceptionType: "",
		},
		{
			name:                "upstream error sets tool.is_error true",
			responseBody:        `{"message":"not found"}`,
			jsonrpcID:           mustMakeID("test-id-004"),
			IsError:             true,
			wantStatusCode:      codes.Unset,
			wantToolErrorReason: `test reason`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracer, spanRecorder := setupTestTracer()
			server := newTestServer(t, "testdata/petstore.openapi.yaml")

			spanName := "test_" + tt.name
			ctx, rootSpan := tracer.Start(context.Background(), spanName)

			var errInfo *errorInfo
			if tt.IsError {
				errInfo = &errorInfo{"test reason"}
			}
			resp := server.mcpResponseHandler(ctx, []byte(tt.responseBody), tt.jsonrpcID, &toolResponseConfig{}, errInfo)
			assert.NotNil(t, resp, "Expected non-nil response")

			rootSpan.End()
			spans := spanRecorder.Ended()

			testSpan, found := findSpanByName(spans, spanName)
			assert.True(t, found, "span not found for %s", spanName)

			// Verify error events match expectations
			assertExceptionEvents(t, testSpan, tt.wantErrorEvent, tt.wantExceptionType)

			// Verify span status
			assert.Equal(t, tt.wantStatusCode, testSpan.Status().Code, "span status")

			assertToolError(t, testSpan, tt.wantToolErrorReason)
		})
	}
}

func TestMcpResponseHandler_ErrorTracing(t *testing.T) {
	tests := []struct {
		name              string
		jsonrpcID         jsonrpc.ID
		config            *toolResponseConfig
		wantStatusCode    codes.Code
		HasException      bool
		wantExceptionType string
	}{
		{
			name:           "nil jsonrpcID",
			jsonrpcID:      jsonrpc.ID{},
			config:         &toolResponseConfig{},
			wantStatusCode: codes.Error,
			HasException:   true,
		},
		{
			name:           "nil config",
			jsonrpcID:      mustMakeID("resp-1"),
			config:         nil,
			wantStatusCode: codes.Error,
			HasException:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracer, spanRecorder := setupTestTracer()
			server := newTestServer(t, "testdata/petstore.openapi.yaml")

			spanName := "test_" + tt.name
			ctx, span := tracer.Start(context.Background(), spanName)
			resp := server.mcpResponseHandler(ctx, []byte(`{"status":"ok"}`), tt.jsonrpcID, tt.config, nil)
			span.End()

			require.NotNil(t, resp)
			recorded, found := findSpanByName(spanRecorder.Ended(), spanName)
			require.True(t, found, "span %q not found", spanName)
			assert.Equal(t, tt.wantStatusCode, recorded.Status().Code, "span status")
			assertExceptionEvents(t, recorded, tt.HasException, tt.wantExceptionType)
		})
	}
}

// setupTestTracer creates a tracer with an in-memory span recorder for testing
func setupTestTracer() (trace.Tracer, *tracetest.SpanRecorder) {
	spanRecorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(spanRecorder),
	)
	otel.SetTracerProvider(tracerProvider)
	tracer := tracerProvider.Tracer("test-tracer")
	return tracer, spanRecorder
}

// getSpanAttribute returns the value of an attribute from a span, or empty string if not found
func getSpanAttribute(span sdktrace.ReadOnlySpan, key string) string {
	for _, attr := range span.Attributes() {
		if string(attr.Key) == key {
			return attr.Value.AsString()
		}
	}
	return ""
}

// getBoolSpanAttribute returns the boolean value of a span attribute, or false if not found.
func getBoolSpanAttribute(span sdktrace.ReadOnlySpan, key string) bool {
	for _, attr := range span.Attributes() {
		if string(attr.Key) == key {
			return attr.Value.AsBool()
		}
	}
	return false
}

// findSpanByName finds a span by name in the recorded spans
func findSpanByName(spans []sdktrace.ReadOnlySpan, name string) (sdktrace.ReadOnlySpan, bool) {
	for _, span := range spans {
		if span.Name() == name {
			return span, true
		}
	}
	return nil, false
}

// assertToolError verifies that tool error attributes are set correctly on a span.
func assertToolError(t *testing.T, span sdktrace.ReadOnlySpan, wantReason string) {
	t.Helper()
	if wantReason == "" {
		return
	}
	assert.True(t, getBoolSpanAttribute(span, attrToolIsError), "tool.is_error should be true")
	assert.Equal(t, wantReason, getSpanAttribute(span, attrToolErrorReason), "tool.error.reason")
}

// assertExceptionEvents verifies exception events match expectations
func assertExceptionEvents(t *testing.T, span sdktrace.ReadOnlySpan, wantErrorEvent bool, wantExceptionType string) {
	if wantErrorEvent {
		assert.NotEmpty(t, span.Events(), "expected error events")
		foundException := false
		for _, event := range span.Events() {
			if event.Name == "exception" {
				foundException = true
				// Verify exception has required attributes
				hasType := false
				hasMessage := false
				var exceptionType string
				for _, attr := range event.Attributes {
					if string(attr.Key) == "exception.type" {
						hasType = true
						exceptionType = attr.Value.AsString()
					}
					if string(attr.Key) == "exception.message" {
						hasMessage = true
					}
				}
				assert.True(t, hasType, "exception event should have exception.type")
				assert.True(t, hasMessage, "exception event should have exception.message")
				if wantExceptionType != "" {
					assert.Equal(t, wantExceptionType, exceptionType, "exception.type")
				}
				break
			}
		}
		assert.True(t, foundException, "expected exception event")
	} else {
		for _, event := range span.Events() {
			assert.NotEqual(t, "exception", event.Name, "unexpected exception event")
		}
	}
}
