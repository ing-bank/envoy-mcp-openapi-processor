package envoy_mcp_openapi_processor

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"go.uber.org/zap"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func loadToolsFromOpenAPI(path string, config *ToolRegistryConfig) (*toolRegistry, error) {
	zap.L().Info("Loading OpenAPI spec", zap.String("path", path))

	ctx := context.Background()
	loader := &openapi3.Loader{Context: ctx}

	spec, err := loader.LoadFromFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to load OpenAPI spec: %w", err)
	}

	// Validate the spec
	if err := spec.Validate(ctx); err != nil {
		zap.L().Warn("OpenAPI spec validation failed", zap.Error(err))
	}

	registry := generateMCPToolsFromSpec(spec, config)
	zap.L().Info("Successfully generated tools from OpenAPI spec", zap.Int("count", registry.Len()))

	return registry, nil
}

func newToolRegistryFromConfig(config *ToolRegistryConfig) (*toolRegistry, error) {
	zap.L().Info("Loading OpenAPI specs matching pattern", zap.String("pattern", config.OpenAPISpecPattern))

	matches, err := filepath.Glob(config.OpenAPISpecPattern)
	if err != nil {
		return nil, fmt.Errorf("invalid glob pattern '%s': %w", config.OpenAPISpecPattern, err)
	}

	if len(matches) == 0 {
		return nil, fmt.Errorf("no files matched pattern: '%s'", config.OpenAPISpecPattern)
	}

	registry := newToolRegistry()
	filesLoaded := 0

	for _, path := range matches {

		info, err := os.Stat(path)
		if err != nil {
			zap.L().Warn("Error accessing path", zap.String("path", path), zap.Error(err))
			continue
		}
		if info.IsDir() {
			continue
		}

		zap.L().Info("Loading OpenAPI spec", zap.String("path", path))

		loaded, err := loadToolsFromOpenAPI(path, config)
		if err != nil {
			zap.L().Warn("Failed to load OpenAPI spec", zap.String("path", path), zap.Error(err))
			continue
		}

		registerLoadedTools(registry, loaded, path)
		filesLoaded++
	}

	if registry.Len() == 0 {
		return nil, fmt.Errorf("no tools loaded from OpenAPI specs matching pattern: %s", config.OpenAPISpecPattern)
	}
	zap.L().Info("Successfully loaded tools from OpenAPI specs", zap.Int("toolCount", registry.Len()), zap.Int("filesLoaded", filesLoaded))
	return registry, nil
}

func registerLoadedTools(registry *toolRegistry, loaded *toolRegistry, specPath string) {
	for _, tool := range loaded.tools {
		config, exists := loaded.configs[tool.Name]
		if !exists {
			continue
		}

		if err := registry.Register(tool, config); err != nil {
			zap.L().Warn("Adding tool to registry failed, skipping", zap.Error(err), zap.String("path", specPath), zap.String("toolName", tool.Name))
		}
	}
}

func generateMCPToolsFromSpec(spec *openapi3.T, config *ToolRegistryConfig) *toolRegistry {
	registry := newToolRegistry()

	host := extractHostFromOpenAPISpec(spec)

	for path, pathItem := range spec.Paths.Map() {
		for method, apiOperation := range pathItem.Operations() {
			if shouldSkipOperation(path, method, apiOperation) {
				continue
			}

			toolName := apiOperation.OperationID
			mcpTool, toolConfig := generateMCPToolFromApiOperation(apiOperation, method, path, host, toolName)

			if mcpTool.OutputSchema == nil || !config.StructuredOutput {
				// StructuredOutput is disabled, remove output schema
				zap.L().Info("Structured output is disabled, ignoring output schema for tool", zap.String("toolName", toolName))
				mcpTool.OutputSchema = nil
			}
			toolConfig.toolResponseConfig.UseStructuredOutput = mcpTool.OutputSchema != nil

			if err := registry.Register(mcpTool, toolConfig); err != nil {
				zap.L().Warn("Adding tool to registry failed, skipping", zap.Error(err), zap.String("toolName", toolName))
			}
		}
	}

	return registry
}

func shouldSkipOperation(path string, method string, apiOperation *openapi3.Operation) bool {
	if apiOperation == nil {
		return true
	}

	if apiOperation.OperationID == "" {
		zap.L().Warn("Skipping operation without operationId",
			zap.String("method", method), zap.String("path", path))
		return true
	}

	if hasSupportedRequestBody(apiOperation) {
		return false
	}

	zap.L().Warn("Skipping operation with unsupported request body content-type",
		zap.String("method", method), zap.String("path", path),
		zap.String("operationId", apiOperation.OperationID))
	return true
}

func hasSupportedRequestBody(op *openapi3.Operation) bool {
	if op.RequestBody == nil {
		return true
	}
	requestBody := op.RequestBody.Value
	if requestBody == nil {
		return true
	}
	for contentType := range requestBody.Content {
		if strings.HasPrefix(contentType, "application/json") {
			return true
		}
	}
	return false
}

func extractHostFromOpenAPISpec(spec *openapi3.T) string {
	if len(spec.Servers) > 0 {
		serverURL := spec.Servers[0].URL
		if parsedURL, err := url.Parse(serverURL); err == nil && parsedURL.Host != "" {
			zap.L().Info("Using host from OpenAPI spec", zap.String("host", parsedURL.Host))
			return parsedURL.Host
		}
		zap.L().Warn("Failed to parse server URL", zap.String("serverURL", serverURL))
	}

	zap.L().Warn("No valid servers found, using localhost")
	return "localhost"
}

func openApiSchemaToMap(schema *openapi3.Schema) map[string]any {
	result := make(map[string]any)

	if schema.Type != nil {
		result["type"] = schema.Type
	}
	if schema.Description != "" {
		result["description"] = schema.Description
	}
	if schema.Enum != nil {
		result["enum"] = schema.Enum
	}
	if len(schema.Required) > 0 {
		result["required"] = schema.Required
	}
	if schema.Properties != nil {
		props := make(map[string]any)
		for name, propRef := range schema.Properties {
			if propRef.Value != nil {
				props[name] = openApiSchemaToMap(propRef.Value)
			}
		}
		result["properties"] = props
	}
	if schema.Items != nil && schema.Items.Value != nil {
		result["items"] = openApiSchemaToMap(schema.Items.Value)
	}

	return result
}

func findJSONSchemaFromResponses(responses *openapi3.Responses) *openapi3.Schema {
	if responses == nil {
		return nil
	}

	var defaultSchema *openapi3.Schema
	var anySchema *openapi3.Schema
	var candidateCodes []int
	candidateSchemas := make(map[int]*openapi3.Schema)

	for status, responseRef := range responses.Map() {
		if responseRef == nil || responseRef.Value == nil {
			continue
		}
		jsonSchema := findJSONSchemaInResponse(responseRef.Value)
		if jsonSchema == nil {
			continue
		}

		statusCode, err := strconv.Atoi(status)
		switch {
		case status == "default":
			if defaultSchema == nil {
				defaultSchema = jsonSchema
			}
		case err == nil && statusCode >= 200 && statusCode <= 299:
			candidateCodes = append(candidateCodes, statusCode)
			candidateSchemas[statusCode] = jsonSchema
		default:
			if anySchema == nil {
				anySchema = jsonSchema
			}
		}
	}

	if len(candidateCodes) > 0 {
		sort.Ints(candidateCodes)
		return candidateSchemas[candidateCodes[0]]
	}
	if defaultSchema != nil {
		return defaultSchema
	}

	return anySchema
}

func findJSONSchemaInResponse(response *openapi3.Response) *openapi3.Schema {
	if response == nil {
		return nil
	}

	for contentType, mediaType := range response.Content {
		if !strings.HasPrefix(contentType, "application/json") {
			continue
		}
		if mediaType == nil || mediaType.Schema == nil {
			continue
		}
		return mediaType.Schema.Value
	}

	return nil
}

func isTopLevelObjectSchema(schema *openapi3.Schema) bool {
	if schema == nil || schema.Type == nil {
		return false
	}
	return strings.Contains(fmt.Sprint(schema.Type), "object")
}

func openApiParameterToSchema(param *openapi3.Parameter) map[string]any {
	propSchema := make(map[string]any)
	if param.Schema != nil && param.Schema.Value != nil {
		propSchema = openApiSchemaToMap(param.Schema.Value)
	}
	// Parameter-level description overrides schema-level description
	if param.Description != "" {
		propSchema["description"] = param.Description
	}
	return propSchema
}

func generateMCPToolFromApiOperation(apiOperation *openapi3.Operation, method, path, host, toolName string) (*mcp.Tool, *toolConfig) {
	inputSchemaProps := make(map[string]any)
	requiredFields := []string{}
	endpointParams := make(map[string]any)
	contentType := ""

	for _, paramRef := range apiOperation.Parameters {
		param := paramRef.Value
		inputSchemaProps[param.Name] = openApiParameterToSchema(param)
		if param.Required {
			requiredFields = append(requiredFields, param.Name)
		}
		endpointParams[param.Name] = map[string]any{
			"in":       param.In,
			"required": param.Required,
		}
	}

	if apiOperation.RequestBody != nil {
		requestBody := apiOperation.RequestBody.Value

		bodySchema := map[string]any{
			"type":        "object",
			"description": "Request body",
		}

		if jsonContent, exists := requestBody.Content["application/json"]; exists {
			contentType = "application/json"
			schema := jsonContent.Schema.Value
			if schema != nil {
				bodySchema = openApiSchemaToMap(schema)
				if _, hasDesc := bodySchema["description"]; !hasDesc {
					bodySchema["description"] = "Request body"
				}
			}
		}

		inputSchemaProps["body"] = bodySchema

		if requestBody.Required {
			requiredFields = append(requiredFields, "body")
		}

	}

	// Create MCP Tool
	inputSchema := map[string]any{
		"type": "object",
	}
	if len(inputSchemaProps) == 0 {
		inputSchema["additionalProperties"] = false
	} else {
		inputSchema["properties"] = inputSchemaProps
		inputSchema["required"] = requiredFields
	}

	var outputSchema map[string]any
	if responseSchema := findJSONSchemaFromResponses(apiOperation.Responses); responseSchema != nil {
		// only object response schemas are supported for now, see issue for details
		// https://github.com/modelcontextprotocol/modelcontextprotocol/issues/834
		if isTopLevelObjectSchema(responseSchema) {
			outputSchema = openApiSchemaToMap(responseSchema)
			if _, hasDesc := outputSchema["description"]; !hasDesc {
				outputSchema["description"] = "API response object"
			}
		} else {
			zap.L().Warn("Only top-level object response schemas are supported, ignoring output schema for tool",
				zap.String("toolName", toolName))
		}
	}

	mcpTool := &mcp.Tool{
		Name:        toolName,
		Title:       apiOperation.Summary,
		Description: apiOperation.Description,
		InputSchema: inputSchema,
	}

	if outputSchema != nil {
		mcpTool.OutputSchema = outputSchema
	}

	// Create toolConfig for the server
	toolConfig := &toolConfig{
		Endpoint: endpoint{
			Host:         host,
			Method:       strings.ToLower(method),
			PathTemplate: path,
			Parameters:   endpointParams,
			ContentType:  contentType,
		},
	}

	return mcpTool, toolConfig
}
