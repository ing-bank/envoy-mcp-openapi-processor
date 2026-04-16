package envoy_mcp_openapi_processor

import (
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewEndpointRequest_BuildsRequestFromArguments(t *testing.T) {
	endpoint := endpoint{
		Method:       "post",
		PathTemplate: "/users/{id}",
		ContentType:  "application/json",
		Parameters: map[string]any{
			"id": map[string]any{
				"in":       openapi3.ParameterInPath,
				"required": true,
			},
			"status": map[string]any{
				"in": openapi3.ParameterInQuery,
			},
			"role": map[string]any{
				"in": openapi3.ParameterInQuery,
			},
			"x-trace-id": map[string]any{
				"in": openapi3.ParameterInHeader,
			},
		},
	}

	request, err := newEndpointRequest(endpoint, map[string]any{
		"id":         "123",
		"role":       []any{"admin", "editor"},
		"status":     "active",
		"x-trace-id": "trace-1",
	})

	require.Nil(t, err)
	require.NotNil(t, request)
	assert.Equal(t, "/users/123?role=admin&role=editor&status=active", request.fullPath())
	assert.Equal(t, "*/*", request.extraHeaders["accept"])
	assert.Equal(t, "application/json", request.extraHeaders["content-type"])
	assert.Equal(t, "trace-1", request.extraHeaders["x-trace-id"])
}

func TestNewEndpointRequest_ReturnsErrorForMissingRequiredArgument(t *testing.T) {
	endpoint := endpoint{
		PathTemplate: "/users/{id}",
		Parameters: map[string]any{
			"id": map[string]any{
				"in":       openapi3.ParameterInPath,
				"required": true,
			},
		},
	}

	request, err := newEndpointRequest(endpoint, map[string]any{})

	assert.Nil(t, request)
	require.NotNil(t, err)
	assert.Equal(t, openapi3.ParameterInPath, err.paramIn)
	assert.Equal(t, "id", err.paramName)
	assert.Equal(t, "required but missing", err.reason)
}

func TestNewEndpointRequest_ReturnsErrorForNestedJSONHeaderArgument(t *testing.T) {
	endpoint := endpoint{
		PathTemplate: "/users",
		Parameters: map[string]any{
			"x-metadata": map[string]any{
				"in": openapi3.ParameterInHeader,
			},
		},
	}

	request, err := newEndpointRequest(endpoint, map[string]any{
		"x-metadata": map[string]any{
			"nested": map[string]any{
				"property": "test",
			},
		},
	})

	assert.Nil(t, request)
	require.NotNil(t, err)
	assert.Equal(t, openapi3.ParameterInHeader, err.paramIn)
	assert.Equal(t, "x-metadata", err.paramName)
	assert.Equal(t, "must be a scalar value", err.reason)
}
