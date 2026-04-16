package envoy_mcp_openapi_processor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"
)

const (
	petstoreHost     = "petstore3.swagger.io"
	fixturePetName   = "doggo"
	fixturePetPhoto  = "https://example.com/doggo.jpg"
	fixturePetStatus = "available"
)

type toolPropertyExpectation struct {
	Description         string
	TypeContains        string
	Enum                []any
	AssertInputProperty func(t *testing.T, property map[string]any)
}

type endpointParameterExpectation struct {
	In string
}

type toolSchemaExpectation struct {
	TypeContains            string
	Description             string
	AssertOutputSchemaExtra func(t *testing.T, outputSchema map[string]any)
}

type toolExpectation struct {
	Host                 string
	Method               string
	PathTemplate         string
	Title                string
	Description          string
	RequiredInputFields  []string
	ForbiddenInputFields []string
	EndpointParameters   map[string]endpointParameterExpectation
	InputProperties      map[string]toolPropertyExpectation
	OutputSchema         *toolSchemaExpectation
}

type bodyExpectation struct {
	Empty       bool
	ContentType string // non-empty implies asserting a non-empty body with this content type
}

type requestExpectation struct {
	Method             string
	Path               string
	Authority          string
	Body               bodyExpectation
	AssertRequestExtra func(t *testing.T, reqResult *mcpProcResponse)
}

type responseExpectation struct {
	ExpectContentTextEq         string
	ExpectContentTextEqUpstream bool
	IsStructured                bool
	IsError                     bool
	AssertResponseExtra         func(t *testing.T, respResult *extProcPb.ProcessingResponse, upstreamBody []byte)
}

type petstoreOperationTranslationCase struct {
	name                    string
	operationID             string
	disableStructuredOutput bool
	requestArguments        map[string]any
	upstreamResponse        []byte
	toolExpectation         toolExpectation
	requestExpectation      requestExpectation
	responseExpectation     responseExpectation
}

func mkAddPetTestCase(t *testing.T) petstoreOperationTranslationCase {
	return petstoreOperationTranslationCase{
		name:        "addPet application/json",
		operationID: "addPet",
		requestArguments: map[string]any{
			"body": petFixture(99),
		},
		upstreamResponse: mustJSONBytes(t, petFixture(99)),
		toolExpectation: toolExpectation{
			Host:                petstoreHost,
			Method:              "post",
			PathTemplate:        "/pet",
			Title:               "Add a new pet to the store.",
			Description:         "Add a new pet to the store.",
			RequiredInputFields: []string{"body"},
			InputProperties: map[string]toolPropertyExpectation{
				"body": {
					TypeContains: "object",
					AssertInputProperty: func(t *testing.T, property map[string]any) {
						t.Helper()
						assertPetSchema(t, property, "addPet body")
					},
				},
			},
			OutputSchema: &toolSchemaExpectation{
				TypeContains: "object",
				AssertOutputSchemaExtra: func(t *testing.T, outputSchema map[string]any) {
					t.Helper()
					assertPetSchema(t, outputSchema, "addPet output")
				},
			},
		},
		requestExpectation: requestExpectation{
			Method:    "POST",
			Path:      "/pet",
			Authority: petstoreHost,
			Body:      bodyExpectation{ContentType: "application/json"},
			AssertRequestExtra: func(t *testing.T, reqResult *mcpProcResponse) {
				t.Helper()
				bodyResp := reqResult.ProcRep.GetRequestBody()
				require.NotNil(t, bodyResp)
				commonResp := bodyResp.GetResponse()
				require.NotNil(t, commonResp)
				var decoded map[string]any
				require.NoError(t, json.Unmarshal(commonResp.GetBodyMutation().GetBody(), &decoded))
				assert.Equal(t, fixturePetName, decoded["name"])
			},
		},
		responseExpectation: responseExpectation{
			ExpectContentTextEqUpstream: true,
			IsStructured:                true,
		},
	}
}

func TestPetstoreProtocolTranslation_OperationCases(t *testing.T) {
	testCases := []petstoreOperationTranslationCase{
		{
			name:             "findPetsByStatus application/json",
			operationID:      "findPetsByStatus",
			requestArguments: map[string]any{"status": "available"},
			upstreamResponse: mustJSONBytes(t, []any{petFixture(1)}),
			toolExpectation: toolExpectation{
				Host:                 petstoreHost,
				Method:               "get",
				PathTemplate:         "/pet/findByStatus",
				Title:                "Finds Pets by status.",
				Description:          "Multiple status values can be provided with comma separated strings.",
				RequiredInputFields:  []string{"status"},
				ForbiddenInputFields: []string{"body"},
				EndpointParameters: map[string]endpointParameterExpectation{
					"status": {In: "query"},
				},
				InputProperties: map[string]toolPropertyExpectation{
					"status": {
						Description:  "Status values that need to be considered for filter",
						TypeContains: "string",
						Enum:         []any{"available", "pending", "sold"},
					},
				},
			},
			requestExpectation: requestExpectation{
				Method:    "GET",
				Path:      "/pet/findByStatus?status=available",
				Authority: petstoreHost,
				Body:      bodyExpectation{Empty: true},
			},
			responseExpectation: responseExpectation{
				ExpectContentTextEqUpstream: true,
				IsStructured:                false, // unwrapped array responses are not allowed to be structured content
			},
		},
		{
			name:             "deletePet api_key header forwarded",
			operationID:      "deletePet",
			requestArguments: map[string]any{"petId": 42, "api_key": "my-secret-key"},
			toolExpectation: toolExpectation{
				Host:                petstoreHost,
				Method:              "delete",
				PathTemplate:        "/pet/{petId}",
				Title:               "Deletes a pet.",
				Description:         "Delete a pet.",
				RequiredInputFields: []string{"petId"},
				EndpointParameters: map[string]endpointParameterExpectation{
					"petId":   {In: "path"},
					"api_key": {In: "header"},
				},
				InputProperties: map[string]toolPropertyExpectation{
					"petId": {
						Description:  "Pet id to delete",
						TypeContains: "integer",
					},
					"api_key": {
						TypeContains: "string",
					},
				},
			},
			requestExpectation: requestExpectation{
				Method:    "DELETE",
				Path:      "/pet/42",
				Authority: petstoreHost,
				Body:      bodyExpectation{Empty: true},
				AssertRequestExtra: func(t *testing.T, reqResult *mcpProcResponse) {
					t.Helper()
					bodyResp := reqResult.ProcRep.GetRequestBody()
					require.NotNil(t, bodyResp)
					commonResp := bodyResp.GetResponse()
					require.NotNil(t, commonResp)
					gotHeaders := headersFromSetHeaders(commonResp.GetHeaderMutation().GetSetHeaders())
					assert.Equal(t, "my-secret-key", gotHeaders["api_key"])
				},
			},
			responseExpectation: responseExpectation{
				ExpectContentTextEqUpstream: true,
				IsStructured:                false,
			},
		},
		{
			name:             "getPetById path parameter",
			operationID:      "getPetById",
			requestArguments: map[string]any{"petId": 123},
			upstreamResponse: mustJSONBytes(t, petFixture(123)),
			toolExpectation: toolExpectation{
				Host:                 petstoreHost,
				Method:               "get",
				PathTemplate:         "/pet/{petId}",
				Title:                "Find pet by ID.",
				Description:          "Returns a single pet.",
				RequiredInputFields:  []string{"petId"},
				ForbiddenInputFields: []string{"body"},
				EndpointParameters: map[string]endpointParameterExpectation{
					"petId": {In: "path"},
				},
				InputProperties: map[string]toolPropertyExpectation{
					"petId": {
						Description:  "ID of pet to return",
						TypeContains: "integer",
					},
				},
				OutputSchema: &toolSchemaExpectation{
					TypeContains: "object",
					AssertOutputSchemaExtra: func(t *testing.T, outputSchema map[string]any) {
						t.Helper()
						assertPetSchema(t, outputSchema, "getPetById output")
					},
				},
			},
			requestExpectation: requestExpectation{
				Method:    "GET",
				Path:      "/pet/123",
				Authority: petstoreHost,
				Body:      bodyExpectation{Empty: true},
			},
			responseExpectation: responseExpectation{
				ExpectContentTextEqUpstream: true,
				IsStructured:                true,
			},
		},
		mkAddPetTestCase(t),
		func() petstoreOperationTranslationCase {
			addPet := mkAddPetTestCase(t)
			addPet.disableStructuredOutput = true
			addPet.name += " unstructured output"
			addPet.responseExpectation.IsStructured = false
			addPet.toolExpectation.OutputSchema = nil
			return addPet
		}(),
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			registry, err := newToolRegistryFromConfig(&ToolRegistryConfig{
				OpenAPISpecPattern: "testdata/petstore.openapi.yaml",
				StructuredOutput:   !tc.disableStructuredOutput,
			})
			require.NoError(t, err)
			require.NotNil(t, registry)
			server := &extProcServer{registry: registry}
			toolConfig := registry.GetConfig(tc.operationID)
			require.NotNil(t, toolConfig, "%s tool config should be registered", tc.operationID)

			tool := findToolByName(registry.Tools(), tc.operationID)
			require.NotNil(t, tool, "%s tool should be registered", tc.operationID)
			assertToolTranslation(t, tool, toolConfig, tc.toolExpectation)

			requestBody := buildToolsCallRequest(t, tc.operationID, tc.requestArguments)
			reqResult := server.mcpRequestHandler(context.Background(), requestBody)
			require.NotNil(t, reqResult)
			require.NotNil(t, reqResult.ProcRep)
			assertRequestTranslation(t, reqResult, tc.requestExpectation)

			respResult := server.mcpResponseHandler(context.Background(), tc.upstreamResponse, reqResult.Id, &toolConfig.toolResponseConfig, nil)
			require.NotNil(t, respResult)
			assertResponseTranslation(t, respResult, tc.upstreamResponse, tc.responseExpectation)
		})
	}
}

func TestPetstoreProtocolTranslation_ToolExecutionErrorPaths(t *testing.T) {
	t.Parallel()

	registry, err := newToolRegistryFromConfig(&ToolRegistryConfig{
		OpenAPISpecPattern: "testdata/petstore.openapi.yaml",
		StructuredOutput:   true,
	})
	require.NoError(t, err)

	server := &extProcServer{registry: registry}

	testCases := []struct {
		name              string
		request           []byte
		wantMessagePrefix string
		setup             func(t *testing.T, registry *toolRegistry)
	}{
		{
			name:              "missing required path parameter",
			request:           []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"getPetById","arguments":{}}}`),
			wantMessagePrefix: `Invalid argument "petId": required but missing`,
		},
		{
			name:              "missing required query parameter",
			request:           []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"findPetsByStatus","arguments":{}}}`),
			wantMessagePrefix: `Invalid argument "status": required but missing`,
		},
		{
			name:              "missing required header parameter",
			request:           []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"deletePet","arguments":{"petId":42}}}`),
			wantMessagePrefix: `Invalid argument "api_key": required but missing`,
			setup: func(t *testing.T, registry *toolRegistry) {
				// this setup is done to keep the original Petstore OpenAPI spec untouched
				t.Helper()
				toolConfig := registry.GetConfig("deletePet")
				require.NotNil(t, toolConfig)
				param, ok := toolConfig.Endpoint.Parameters["api_key"].(map[string]any)
				require.True(t, ok)
				param["required"] = true
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setup != nil {
				tc.setup(t, registry)
			}

			result := server.mcpRequestHandler(context.Background(), tc.request)
			require.NotNil(t, result)
			require.NotNil(t, result.ProcRep)

			immediate := result.ProcRep.GetImmediateResponse()
			require.NotNil(t, immediate, "error response should be immediate")

			// All JSON-RPC responses use HTTP 200
			require.NotNil(t, immediate.GetStatus())
			assert.Equal(t, typev3.StatusCode_OK, immediate.GetStatus().GetCode())
			assertToolExecutionError(t, immediate.GetBody(), tc.wantMessagePrefix)
		})
	}
}

func TestPetstoreProtocolTranslation_StructuredOutputDowngradesForInvalidJSON(t *testing.T) {
	t.Parallel()

	registry, err := newToolRegistryFromConfig(&ToolRegistryConfig{
		OpenAPISpecPattern: "testdata/petstore.openapi.yaml",
		StructuredOutput:   true,
	})
	require.NoError(t, err)

	server := &extProcServer{registry: registry}
	toolConfig := registry.GetConfig("addPet")
	require.NotNil(t, toolConfig)
	require.True(t, toolConfig.toolResponseConfig.UseStructuredOutput)

	upstreamBody := []byte(`not-json-upstream-body`)
	response := server.mcpResponseHandler(context.Background(), upstreamBody, mustMakeID("test-id-structured-downgrade"), &toolConfig.toolResponseConfig, nil)
	require.NotNil(t, response)

	assertResponseTranslation(t, response, upstreamBody, responseExpectation{
		ExpectContentTextEqUpstream: true,
		IsStructured:                false,
		IsError:                     false,
	})
}

func TestPetstoreProtocolTranslation_ProcessResponseHeadersEOS(t *testing.T) {
	t.Parallel()

	registry, err := newToolRegistryFromConfig(&ToolRegistryConfig{
		OpenAPISpecPattern: "testdata/petstore.openapi.yaml",
		StructuredOutput:   true,
	})
	require.NoError(t, err)

	server := &extProcServer{registry: registry}

	requestBody := buildToolsCallRequest(t, "getPetById", map[string]any{"petId": 123})
	stream := &fakeProcessStream{
		ctx: context.Background(),
		requests: []*extProcPb.ProcessingRequest{
			{
				Request: &extProcPb.ProcessingRequest_RequestBody{
					RequestBody: &extProcPb.HttpBody{Body: requestBody},
				},
			},
			{
				Request: &extProcPb.ProcessingRequest_ResponseHeaders{
					ResponseHeaders: &extProcPb.HttpHeaders{
						EndOfStream: true,
						Headers: &corev3.HeaderMap{Headers: []*corev3.HeaderValue{
							{Key: ":status", RawValue: []byte("204")},
						}},
					},
				},
			},
		},
	}

	require.NoError(t, server.Process(stream))
	require.Len(t, stream.sentResponses, 2)

	eosResponse := stream.sentResponses[1].GetResponseHeaders()
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

func TestPetstoreProtocolTranslation_ResponseWithoutBodyMarksAPIFailuresAsToolErrors(t *testing.T) {
	t.Parallel()

	registry, err := newToolRegistryFromConfig(&ToolRegistryConfig{
		OpenAPISpecPattern: "testdata/petstore.openapi.yaml",
		StructuredOutput:   true,
	})
	require.NoError(t, err)

	server := &extProcServer{registry: registry}

	requestBody := buildToolsCallRequest(t, "getPetById", map[string]any{"petId": 123})
	stream := &fakeProcessStream{
		ctx: context.Background(),
		requests: []*extProcPb.ProcessingRequest{
			{
				Request: &extProcPb.ProcessingRequest_RequestBody{
					RequestBody: &extProcPb.HttpBody{Body: requestBody},
				},
			},
			{
				Request: &extProcPb.ProcessingRequest_ResponseHeaders{
					ResponseHeaders: &extProcPb.HttpHeaders{
						EndOfStream: true,
						Headers: &corev3.HeaderMap{Headers: []*corev3.HeaderValue{
							{Key: ":status", RawValue: []byte("500")},
						}},
					},
				},
			},
		},
	}

	require.NoError(t, server.Process(stream))
	require.Len(t, stream.sentResponses, 2)

	eosResponse := stream.sentResponses[1].GetResponseHeaders()
	require.NotNil(t, eosResponse)
	common := eosResponse.GetResponse()
	require.NotNil(t, common)

	jsonrpcResponse := decodeJSONRPCResponseBody(t, common.GetBodyMutation().GetBody())
	result, ok := jsonrpcResponse["result"].(map[string]any)
	require.True(t, ok)
	isError, ok := result["isError"].(bool)
	require.True(t, ok)
	assert.True(t, isError)
}

func TestPetstoreProtocolTranslation_ResponseBodyMarksAPIFailuresAsToolErrors(t *testing.T) {
	t.Parallel()

	registry, err := newToolRegistryFromConfig(&ToolRegistryConfig{
		OpenAPISpecPattern: "testdata/petstore.openapi.yaml",
		StructuredOutput:   true,
	})
	require.NoError(t, err)

	server := &extProcServer{registry: registry}
	toolConfig := registry.GetConfig("getPetById")
	require.NotNil(t, toolConfig)

	upstreamBody := []byte(`{"message":"upstream failed"}`)
	response := server.mcpResponseHandler(context.Background(), upstreamBody, mustMakeID("test-id-api-failure"), &toolConfig.toolResponseConfig, httpStatusToErrorInfo(500))
	require.NotNil(t, response)

	assertResponseTranslation(t, response, upstreamBody, responseExpectation{
		ExpectContentTextEqUpstream: true,
		IsStructured:                false,
		IsError:                     true,
	})
}

func TestPetstoreProtocolTranslation_ResponseBodyMarksUnsetStatusAsToolError(t *testing.T) {
	t.Parallel()

	registry, err := newToolRegistryFromConfig(&ToolRegistryConfig{
		OpenAPISpecPattern: "testdata/petstore.openapi.yaml",
		StructuredOutput:   true,
	})
	require.NoError(t, err)

	server := &extProcServer{registry: registry}
	toolConfig := registry.GetConfig("getPetById")
	require.NotNil(t, toolConfig)

	upstreamBody := []byte(`{"message":"status missing"}`)
	response := server.mcpResponseHandler(context.Background(), upstreamBody, mustMakeID("test-id-status-unset"), &toolConfig.toolResponseConfig, httpStatusToErrorInfo(0))
	require.NotNil(t, response)

	assertResponseTranslation(t, response, upstreamBody, responseExpectation{
		ExpectContentTextEqUpstream: true,
		IsStructured:                false,
		IsError:                     true,
	})
}

func petFixture(id int) map[string]any {
	return map[string]any{
		"id":        id,
		"name":      fixturePetName,
		"photoUrls": []any{fixturePetPhoto},
		"status":    fixturePetStatus,
	}
}

func mustJSONBytes(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	require.NoError(t, err)
	return body
}

func assertToolTranslation(t *testing.T, tool *mcp.Tool, toolConfig *toolConfig, expected toolExpectation) {
	t.Helper()

	assert.Equal(t, expected.Host, toolConfig.Endpoint.Host)
	assert.Equal(t, expected.Method, toolConfig.Endpoint.Method)
	assert.Equal(t, expected.PathTemplate, toolConfig.Endpoint.PathTemplate)

	for paramName, paramExpectation := range expected.EndpointParameters {
		endpointParam, ok := toolConfig.Endpoint.Parameters[paramName].(map[string]any)
		require.True(t, ok, "%s endpoint parameter should be present", paramName)
		if paramExpectation.In != "" {
			assert.Equal(t, paramExpectation.In, endpointParam["in"])
		}
	}

	assert.Equal(t, expected.Title, tool.Title)
	assert.Equal(t, expected.Description, tool.Description)

	inputSchema, ok := tool.InputSchema.(map[string]any)
	require.True(t, ok, "input schema should be a map")
	assert.Contains(t, fmt.Sprint(inputSchema["type"]), "object")

	inputProps, ok := inputSchema["properties"].(map[string]any)
	require.True(t, ok, "input schema should have properties")
	for _, forbidden := range expected.ForbiddenInputFields {
		assert.NotContains(t, inputProps, forbidden, "%s should not be exposed in input schema", forbidden)
	}

	for inputName, inputExpectation := range expected.InputProperties {
		inputProperty, ok := inputProps[inputName].(map[string]any)
		require.True(t, ok, "input schema should include %s parameter", inputName)

		if inputExpectation.Description != "" {
			assert.Equal(t, inputExpectation.Description, inputProperty["description"])
		}

		if inputExpectation.TypeContains != "" {
			assert.Contains(t, fmt.Sprint(inputProperty["type"]), inputExpectation.TypeContains)
		}

		if len(inputExpectation.Enum) > 0 {
			actualEnum, ok := inputProperty["enum"].([]any)
			require.True(t, ok, "%s input parameter should include enum values", inputName)
			assert.ElementsMatch(t, inputExpectation.Enum, actualEnum)
		}

		if inputExpectation.AssertInputProperty != nil {
			inputExpectation.AssertInputProperty(t, inputProperty)
		}
	}

	if len(expected.RequiredInputFields) > 0 {
		required, ok := asStringSlice(inputSchema["required"])
		require.True(t, ok, "input schema should define required fields")
		assert.ElementsMatch(t, expected.RequiredInputFields, required)
	}

	if expected.OutputSchema != nil {
		schemaExpectation := expected.OutputSchema
		outputSchema, ok := tool.OutputSchema.(map[string]any)
		require.True(t, ok, "output schema should be a map")
		if schemaExpectation.TypeContains != "" {
			assert.Contains(t, fmt.Sprint(outputSchema["type"]), schemaExpectation.TypeContains)
		}
		if schemaExpectation.Description != "" {
			assert.Equal(t, schemaExpectation.Description, outputSchema["description"])
		}
		if schemaExpectation.AssertOutputSchemaExtra != nil {
			schemaExpectation.AssertOutputSchemaExtra(t, outputSchema)
		}
	} else {
		assert.Nil(t, tool.OutputSchema, "output schema should be nil")
	}
}

func assertRequestTranslation(t *testing.T, reqResult *mcpProcResponse, expected requestExpectation) {
	t.Helper()

	bodyResp := reqResult.ProcRep.GetRequestBody()
	require.NotNil(t, bodyResp, "tools/call should produce a request body mutation response")

	commonResp := bodyResp.GetResponse()
	require.NotNil(t, commonResp)
	require.NotNil(t, commonResp.GetHeaderMutation())

	gotHeaders := headersFromSetHeaders(commonResp.GetHeaderMutation().GetSetHeaders())
	if expected.Method != "" {
		assert.Equal(t, expected.Method, gotHeaders[":method"])
	}
	if expected.Path != "" {
		assert.Equal(t, expected.Path, gotHeaders[":path"])
	}
	if expected.Authority != "" {
		assert.Equal(t, expected.Authority, gotHeaders[":authority"])
	}

	require.NotNil(t, commonResp.GetBodyMutation())
	if expected.Body.Empty {
		assert.Empty(t, commonResp.GetBodyMutation().GetBody(), "request body should be empty")
		assert.Equal(t, "0", gotHeaders["content-length"])
	}
	if expected.Body.ContentType != "" {
		assert.NotEmpty(t, commonResp.GetBodyMutation().GetBody(), "request body should not be empty")
		assert.Equal(t, expected.Body.ContentType, gotHeaders["content-type"])
	}

	if expected.AssertRequestExtra != nil {
		expected.AssertRequestExtra(t, reqResult)
	}
}

func assertResponseTranslation(t *testing.T, respResult *extProcPb.ProcessingResponse, upstreamBody []byte, expected responseExpectation) {
	t.Helper()

	responseBody := respResult.GetResponseBody()
	require.NotNil(t, responseBody, "mcpResponseHandler should produce response body mutation")

	commonResp := responseBody.GetResponse()
	require.NotNil(t, commonResp)
	require.NotNil(t, commonResp.GetHeaderMutation())

	gotHeaders := headersFromSetHeaders(commonResp.GetHeaderMutation().GetSetHeaders())
	assert.Equal(t, "application/json", gotHeaders["content-type"])

	jsonrpcResponse := decodeJSONRPCResponseBody(t, commonResp.GetBodyMutation().GetBody())
	if errObj, hasErr := jsonrpcResponse["error"]; hasErr {
		t.Fatalf("jsonrpc response contains error: %v", errObj)
	}
	result, ok := jsonrpcResponse["result"].(map[string]any)
	require.True(t, ok, "jsonrpc response should include result object")

	if expected.ExpectContentTextEqUpstream || expected.ExpectContentTextEq != "" {
		content, ok := result["content"].([]any)
		require.True(t, ok, "result should include content")
		require.NotEmpty(t, content)

		firstContent, ok := content[0].(map[string]any)
		require.True(t, ok)

		if expected.ExpectContentTextEqUpstream {
			assert.Equal(t, string(upstreamBody), firstContent["text"])
		} else {
			assert.Equal(t, expected.ExpectContentTextEq, firstContent["text"])
		}
	}

	if expected.IsStructured {
		var expectedStructured any
		require.NoError(t, json.Unmarshal(upstreamBody, &expectedStructured))
		actualStructured, exists := result["structuredContent"]
		require.True(t, exists, "structuredContent should be present")
		assert.Equal(t, expectedStructured, actualStructured)
	} else {
		structuredContent, exists := result["structuredContent"]
		assert.True(t, !exists || structuredContent == nil, "structuredContent should be absent or nil")
	}

	isError, ok := result["isError"].(bool)
	// json.Marshal with omitempty omits false boolean fields, so we only require isError to be present if expected.IsError is true
	require.Equal(t, expected.IsError, ok, "result should include isError boolean only if expected.IsError is true")
	assert.Equal(t, expected.IsError, isError, "result isError field should match expected.IsError")

	if expected.AssertResponseExtra != nil {
		expected.AssertResponseExtra(t, respResult, upstreamBody)
	}
}

func asStringSlice(value any) ([]string, bool) {
	switch v := value.(type) {
	case []string:
		return v, true
	case []any:
		result := make([]string, len(v))
		for i, rawValue := range v {
			stringValue, ok := rawValue.(string)
			if !ok {
				return nil, false
			}
			result[i] = stringValue
		}
		return result, true
	default:
		return nil, false
	}
}

func buildToolsCallRequest(t *testing.T, operationID string, arguments map[string]any) []byte {
	t.Helper()

	request := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      operationID,
			"arguments": arguments,
		},
	}

	body, err := json.Marshal(request)
	require.NoError(t, err)
	return body
}

func decodeJSONRPCResponseBody(t *testing.T, body []byte) map[string]any {
	t.Helper()

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(body, &decoded))
	return decoded
}

func headersFromSetHeaders(headers []*corev3.HeaderValueOption) map[string]string {
	result := make(map[string]string, len(headers))
	for _, header := range headers {
		key := header.GetHeader().GetKey()
		if raw := header.GetHeader().GetRawValue(); len(raw) > 0 {
			result[key] = string(raw)
		} else {
			result[key] = header.GetHeader().GetValue()
		}
	}
	return result
}

func findToolByName(tools []*mcp.Tool, name string) *mcp.Tool {
	for _, tool := range tools {
		if tool.Name == name {
			return tool
		}
	}
	return nil
}

func assertToolExecutionError(t *testing.T, body []byte, wantMessagePrefix string) {
	t.Helper()

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(body, &decoded), "body should be valid JSON")
	assert.NotContains(t, decoded, "error", "tool execution error must not contain a JSON-RPC 'error' field")

	resultObj, ok := decoded["result"].(map[string]any)
	require.True(t, ok, "response should contain 'result' field")

	isError, ok := resultObj["isError"].(bool)
	require.True(t, ok, "result should contain 'isError' boolean")
	assert.True(t, isError, "isError should be true for tool execution errors")

	content, ok := resultObj["content"].([]any)
	require.True(t, ok, "result should contain 'content' array")
	require.NotEmpty(t, content)

	first, ok := content[0].(map[string]any)
	require.True(t, ok)
	text, ok := first["text"].(string)
	require.True(t, ok, "first content item should have 'text' field")
	assert.True(t, strings.HasPrefix(text, wantMessagePrefix),
		"expected text to start with %q, got %q", wantMessagePrefix, text)
}

type fakeProcessStream struct {
	extProcPb.ExternalProcessor_ProcessServer
	ctx           context.Context
	requests      []*extProcPb.ProcessingRequest
	sentResponses []*extProcPb.ProcessingResponse
	readIndex     int
}

func (s *fakeProcessStream) Recv() (*extProcPb.ProcessingRequest, error) {
	if s.readIndex >= len(s.requests) {
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
