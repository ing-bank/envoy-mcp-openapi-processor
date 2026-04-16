package envoy_mcp_openapi_processor

import (
	"fmt"
	"os"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewToolRegistryFromOpenApiSpec_MultipleMatches(t *testing.T) {
	registry := mkDefaultToolRegistry(t)

	assert.Equal(t, 4, registry.Len())

	assert.NotNil(t, registry.GetConfig("listUsers"))
	assert.NotNil(t, registry.GetConfig("getUser"))
	assert.NotNil(t, registry.GetConfig("listProducts"))
	assert.NotNil(t, registry.GetConfig("createProduct"))
}

func TestNewToolRegistryFromOpenApiSpec_EmptyPattern(t *testing.T) {
	registry, err := newToolRegistryFromConfig(&ToolRegistryConfig{})

	assert.ErrorContains(t, err, "no files matched pattern: ''")
	assert.Nil(t, registry)
}

func TestNewToolRegistryFromOpenApiSpec_NoMatches(t *testing.T) {
	tmpDir := t.TempDir()
	specPath := tmpDir + "/*.openapi.yaml"
	registry, err := newToolRegistryFromConfig(&ToolRegistryConfig{OpenAPISpecPattern: specPath})

	assert.ErrorContains(t, err, "no files matched pattern: '"+specPath+"'")
	assert.Nil(t, registry)
}

func TestNewToolRegistryFromOpenApiSpec_InvalidGlobPattern(t *testing.T) {
	registry, err := newToolRegistryFromConfig(&ToolRegistryConfig{OpenAPISpecPattern: "[invalid"})

	assert.ErrorContains(t, err, "syntax error in pattern")
	assert.Nil(t, registry)
}

func TestNewToolRegistryFromOpenApiSpec_SkipsDirectories(t *testing.T) {
	tmpDir := t.TempDir()

	require.NoError(t, os.Mkdir(tmpDir+"/subdir.openapi.yaml", 0700))

	spec1, err := os.ReadFile("testdata/minimal-users.openapi.yaml")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(tmpDir+"/valid.openapi.yaml", spec1, 0600))

	registry, err := newToolRegistryFromConfig(&ToolRegistryConfig{OpenAPISpecPattern: tmpDir + "/*.openapi.yaml"})

	require.NoError(t, err)
	// Should only load from the file containing spec1, not the directory
	assert.Equal(t, 2, registry.Len())

	assert.NotNil(t, registry.GetConfig("listUsers"))
	assert.NotNil(t, registry.GetConfig("getUser"))
}

func TestGenerateMCPToolsFromSpec_SkipsInvalidOperationIDs(t *testing.T) {
	spec := &openapi3.T{
		Paths: &openapi3.Paths{},
	}
	spec.Paths.Set("/valid", &openapi3.PathItem{
		Get: &openapi3.Operation{OperationID: "listUsers"},
	})
	spec.Paths.Set("/invalid", &openapi3.PathItem{
		Post: &openapi3.Operation{OperationID: "list users"},
	})

	registry := generateMCPToolsFromSpec(spec, &ToolRegistryConfig{StructuredOutput: true})

	assert.Equal(t, 1, registry.Len())
	assert.NotNil(t, registry.GetConfig("listUsers"))
	assert.Nil(t, registry.GetConfig("list users"))
	tool := findToolByName(registry.Tools(), "listUsers")
	require.NotNil(t, tool)
	assert.Equal(t, "listUsers", tool.Name)
}

func TestNewToolRegistryFromOpenApiSpec_NoParameterToolHasClosedEmptyObjectInputSchema(t *testing.T) {
	registry := mkToolRegistry(t, "testdata/minimal-users.openapi.yaml")

	tool := findToolByName(registry.Tools(), "listUsers")
	require.NotNil(t, tool)

	inputSchema, ok := tool.InputSchema.(map[string]any)
	require.True(t, ok, "input schema should be a map")
	assert.Equal(t, map[string]any{
		"type":                 "object",
		"additionalProperties": false,
	}, inputSchema)
}

func assertPetSchema(t *testing.T, schema map[string]any, label string) {
	t.Helper()

	assert.Contains(t, fmt.Sprint(schema["type"]), "object")

	props, ok := schema["properties"].(map[string]any)
	require.True(t, ok, "%s should have properties", label)

	expectedPetProperties := []string{"id", "name", "category", "photoUrls", "tags", "status"}
	assert.Equal(t, len(expectedPetProperties), len(props), "%s should have exactly %d properties from Pet schema", label, len(expectedPetProperties))
	for _, prop := range expectedPetProperties {
		assert.Contains(t, props, prop, "%s should have Pet schema property '%s'", label, prop)
	}

	// Verify required fields match Pet schema's required: [name, photoUrls]
	schemaRequired, ok := schema["required"].([]string)
	require.True(t, ok, "%s should have required fields from Pet schema", label)
	assert.ElementsMatch(t, []string{"name", "photoUrls"}, schemaRequired)

	// Verify individual property types match the Pet schema definitions
	idProp, ok := props["id"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, fmt.Sprint(idProp["type"]), "integer")

	nameProp, ok := props["name"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, fmt.Sprint(nameProp["type"]), "string")

	categoryProp, ok := props["category"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, fmt.Sprint(categoryProp["type"]), "object")
	categoryProps, ok := categoryProp["properties"].(map[string]any)
	require.True(t, ok, "%s category should have nested properties from Category schema", label)
	assert.Contains(t, categoryProps, "id")
	assert.Contains(t, categoryProps, "name")

	photoUrlsProp, ok := props["photoUrls"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, fmt.Sprint(photoUrlsProp["type"]), "array")
	photoUrlsItems, ok := photoUrlsProp["items"].(map[string]any)
	require.True(t, ok, "%s photoUrls should have items schema", label)
	assert.Contains(t, fmt.Sprint(photoUrlsItems["type"]), "string")

	tagsProp, ok := props["tags"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, fmt.Sprint(tagsProp["type"]), "array")
	tagsItems, ok := tagsProp["items"].(map[string]any)
	require.True(t, ok, "%s tags should have items schema", label)
	assert.Contains(t, fmt.Sprint(tagsItems["type"]), "object")

	statusProp, ok := props["status"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, fmt.Sprint(statusProp["type"]), "string")
	assert.Equal(t, "pet status in the store", statusProp["description"])
	statusEnum, ok := statusProp["enum"].([]any)
	require.True(t, ok, "%s status should have enum values", label)
	assert.ElementsMatch(t, []any{"available", "pending", "sold"}, statusEnum)
}

func TestNewToolRegistryFromOpenApiSpec_PetstorePostWithRequestAndResponseSchema(t *testing.T) {
	registry := mkToolRegistry(t, "testdata/petstore.openapi.yaml")

	// addPet is a POST operation with both request body and response schema
	toolConfig := registry.GetConfig("addPet")
	require.NotNil(t, toolConfig, "addPet tool should be registered")

	var addPetTool *mcp.Tool
	for _, tool := range registry.Tools() {
		if tool.Name == "addPet" {
			addPetTool = tool
			break
		}
	}
	require.NotNil(t, addPetTool)

	assert.Equal(t, "Add a new pet to the store.", addPetTool.Title)
	assert.Equal(t, "Add a new pet to the store.", addPetTool.Description)

	// Verify input schema has a "body" property for the request body
	inputSchema, ok := addPetTool.InputSchema.(map[string]any)
	require.True(t, ok, "input schema should be a map")

	props, ok := inputSchema["properties"].(map[string]any)
	require.True(t, ok, "input schema should have properties")

	bodyProp, ok := props["body"].(map[string]any)
	require.True(t, ok, "input schema should have a 'body' property for the request body")

	// Verify "body" is in the required fields
	required, ok := inputSchema["required"].([]string)
	require.True(t, ok, "input schema should have required fields")
	assert.Contains(t, required, "body")

	// Verify body schema matches the Pet schema
	assertPetSchema(t, bodyProp, "body")

	// Verify output schema matches the Pet schema
	outputSchema, ok := addPetTool.OutputSchema.(map[string]any)
	require.True(t, ok, "output schema should be a map")
	assertPetSchema(t, outputSchema, "output")
}

func TestNewToolRegistryFromOpenApiSpec_StructuredOutputDisabled(t *testing.T) {
	registry := mkToolRegistryFromConfig(t, &ToolRegistryConfig{
		OpenAPISpecPattern: "testdata/petstore.openapi.yaml",
		StructuredOutput:   false,
	})

	toolConfig := registry.GetConfig("addPet")
	require.NotNil(t, toolConfig, "addPet tool should be registered")

	var addPetTool *mcp.Tool
	for _, tool := range registry.Tools() {
		if tool.Name == "addPet" {
			addPetTool = tool
			break
		}
	}
	require.NotNil(t, addPetTool)

	assert.Nil(t, addPetTool.OutputSchema, "output schema should be nil when structured output is disabled")
}

func TestNewToolRegistryFromOpenApiSpec_ArrayResponseHasNoOutputSchema(t *testing.T) {
	registry := mkToolRegistry(t, "testdata/petstore.openapi.yaml")

	var findPetsByStatusTool *mcp.Tool
	for _, tool := range registry.Tools() {
		if tool.Name == "findPetsByStatus" {
			findPetsByStatusTool = tool
			break
		}
	}
	require.NotNil(t, findPetsByStatusTool)
	assert.Nil(t, findPetsByStatusTool.OutputSchema, "output schema should be nil for top-level array responses")
}
