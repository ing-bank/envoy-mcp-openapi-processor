package envoy_mcp_openapi_processor

import (
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mkDefaultToolRegistry(t *testing.T) *toolRegistry {
	return mkToolRegistry(t, "testdata/minimal-*.openapi.yaml")
}

func mkToolRegistry(t *testing.T, openApiSpecPattern string) *toolRegistry {
	return mkToolRegistryFromConfig(t, &ToolRegistryConfig{
		OpenAPISpecPattern: openApiSpecPattern,
		StructuredOutput:   true,
	})
}

func mkToolRegistryFromConfig(t *testing.T, config *ToolRegistryConfig) *toolRegistry {
	registry, err := newToolRegistryFromConfig(config)
	require.NoError(t, err)
	require.NotNil(t, registry)
	return registry
}

func TestToolRegistry_FilterByAllowlist_FiltersTools(t *testing.T) {
	registry := mkDefaultToolRegistry(t)

	filtered, err := registry.FilterByAllowlist([]string{"listUsers", "listProducts"})
	require.NoError(t, err)

	assert.Equal(t, 4, registry.Len(), "original registry should remain unchanged")
	assert.Equal(t, 2, filtered.Len(), "filtered registry should contain only allowed tools")

	assert.NotNil(t, filtered.GetConfig("listUsers"))
	assert.Nil(t, filtered.GetConfig("getUser"))
	assert.NotNil(t, filtered.GetConfig("listProducts"))
	assert.Nil(t, filtered.GetConfig("createProduct"))
}

func TestToolRegistry_FilterByAllowlist_NonExistentToolInAllowlist(t *testing.T) {
	registry := mkDefaultToolRegistry(t)

	// Mix of existing and non-existing tools should return an error
	filtered, err := registry.FilterByAllowlist([]string{"listUsers", "nonexistent-tool"})

	assert.Nil(t, filtered)
	assert.EqualError(t, err, "tools in allowlist not found in registry: nonexistent-tool")
}

func TestToolRegistry_FilterByAllowlist_AllToolsFilteredOut(t *testing.T) {
	registry := mkDefaultToolRegistry(t)

	filtered, err := registry.FilterByAllowlist([]string{"nonexistent-tool-1", "nonexistent-tool-2"})

	assert.Nil(t, filtered)
	assert.EqualError(t, err, "tools in allowlist not found in registry: nonexistent-tool-1, nonexistent-tool-2")
}

func TestToolRegistry_String(t *testing.T) {
	registry := newToolRegistry()

	require.NoError(t, registry.Register(
		&mcp.Tool{Name: "listUsers"},
		&toolConfig{Endpoint: endpoint{Host: "api.example.com", Method: "get", PathTemplate: "/users"}},
	))
	require.NoError(t, registry.Register(
		&mcp.Tool{Name: "createUser"},
		&toolConfig{Endpoint: endpoint{Host: "api.example.com", Method: "post", PathTemplate: "/users"}},
	))

	result := registry.String()

	assert.Equal(t, "[listUsers => GET api.example.com/users, createUser => POST api.example.com/users]", result)
}

func TestToolRegistry_FilterByAllowlist_EmptyAllowlistReturnsError(t *testing.T) {
	registry := mkDefaultToolRegistry(t)

	filtered, err := registry.FilterByAllowlist(nil)
	assert.Nil(t, filtered)
	assert.EqualError(t, err, "allowlist must not be empty")

	filtered2, err := registry.FilterByAllowlist([]string{})
	assert.Nil(t, filtered2)
	assert.EqualError(t, err, "allowlist must not be empty")
}

func TestToolRegistry_Register_RejectsInvalidToolNames(t *testing.T) {
	testCases := []struct {
		name        string
		toolName    string
		errContains string
	}{
		{
			name:        "empty name",
			toolName:    "",
			errContains: "must be between 1 and 128 characters",
		},
		{
			name:        "too long",
			toolName:    strings.Repeat("a", 129),
			errContains: "must be between 1 and 128 characters",
		},
		{
			name:        "contains space",
			toolName:    "list users",
			errContains: "contains invalid character \" \"",
		},
		{
			name:        "contains comma",
			toolName:    "list,users",
			errContains: "contains invalid character \",\"",
		},
		{
			name:        "contains unicode",
			toolName:    "listUserß",
			errContains: "contains invalid character \"ß\"",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			registry := newToolRegistry()

			err := registry.Register(
				&mcp.Tool{Name: tc.toolName},
				&toolConfig{Endpoint: endpoint{Host: "api.example.com", Method: "get", PathTemplate: "/users"}},
			)

			require.Error(t, err)
			assert.ErrorContains(t, err, tc.errContains)
			assert.Equal(t, 0, registry.Len())
		})
	}
}

func TestToolRegistry_Register_AllowsSpecCompliantToolNames(t *testing.T) {
	registry := newToolRegistry()
	toolName := "Users.list_v2-alpha.1"

	err := registry.Register(
		&mcp.Tool{Name: toolName},
		&toolConfig{Endpoint: endpoint{Host: "api.example.com", Method: "get", PathTemplate: "/users"}},
	)

	require.NoError(t, err)
	assert.Equal(t, 1, registry.Len())
	assert.NotNil(t, registry.GetConfig(toolName))
}
