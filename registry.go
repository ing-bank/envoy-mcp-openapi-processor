package envoy_mcp_openapi_processor

import (
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"
)

type endpoint struct {
	Host         string
	Method       string
	PathTemplate string
	Parameters   map[string]any
	ContentType  string
}

func (e *endpoint) String() string {
	return fmt.Sprintf("%s %s%s", strings.ToUpper(e.Method), e.Host, e.PathTemplate)
}

func (e *endpoint) supportsBody() bool {
	method := strings.ToLower(e.Method)
	return method == "post" || method == "put" || method == "patch"
}

type toolConfig struct {
	Endpoint           endpoint
	toolResponseConfig toolResponseConfig
}

// toolResponseConfig defines how tool responses are processed.
type toolResponseConfig struct {
	UseStructuredOutput bool
}

// ToolRegistryConfig defines the configuration for creating a toolRegistry from OpenAPI specs.
type ToolRegistryConfig struct {
	// OpenAPISpecPattern is a glob pattern to match OpenAPI spec files for loading tools.
	// For example, "specs/*.yaml" would load all YAML files in the specs directory.
	OpenAPISpecPattern string
	// ToolAllowlist is an optional list of tool names to allow. If empty, all tools loaded from the OpenAPI specs will be allowed.
	ToolAllowlist []string
	// StructuredOutput indicates whether to enable structured output for tools,
	// which allows using outputSchema for response validation and structured content in tool responses.
	StructuredOutput bool
}

type toolRegistry struct {
	tools   []*mcp.Tool
	configs map[string]*toolConfig
}

const maxToolNameLength = 128

func newToolRegistry() *toolRegistry {
	return &toolRegistry{
		tools:   make([]*mcp.Tool, 0),
		configs: make(map[string]*toolConfig),
	}
}

// Len returns the number of tools in the registry
func (r *toolRegistry) Len() int {
	return len(r.tools)
}

// Register adds a tool and its configuration to the registry.
// Returns an error if the tool is nil or if tool name validation fails.
func (r *toolRegistry) Register(tool *mcp.Tool, config *toolConfig) error {
	if tool == nil {
		return fmt.Errorf("tool cannot be nil")
	}
	if config == nil {
		return fmt.Errorf("config cannot be nil")
	}
	if err := validateToolName(tool.Name); err != nil {
		return err
	}
	if _, exists := r.configs[tool.Name]; exists {
		return fmt.Errorf("tool '%s' already registered", tool.Name)
	}
	r.tools = append(r.tools, tool)
	r.configs[tool.Name] = config
	return nil
}

func validateToolName(name string) error {
	nameLength := len(name)
	if nameLength < 1 || nameLength > maxToolNameLength {
		return fmt.Errorf("tool name %q must be between 1 and %d characters", name, maxToolNameLength)
	}

	for _, r := range name {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			continue
		}
		if r == '_' || r == '-' || r == '.' {
			continue
		}
		return fmt.Errorf("tool name %q contains invalid character %q; See MCP spec for support tool name format", name, string(r))
	}

	return nil
}

// GetConfig returns a pointer to the configuration for the given tool name.
// Returns nil if the tool is not found.
func (r *toolRegistry) GetConfig(name string) *toolConfig {
	return r.configs[name]
}

// Tools returns the tools in the registry.
// The returned slice should be treated as read-only. Callers must not modify
// the slice or its contents
func (r *toolRegistry) Tools() []*mcp.Tool {
	return r.tools
}

func (r *toolRegistry) String() string {
	entries := make([]string, 0, len(r.tools))
	for _, tool := range r.tools {
		if config, exists := r.configs[tool.Name]; exists {
			entries = append(entries, fmt.Sprintf("%s => %s", tool.Name, &config.Endpoint))
		}
	}
	return "[" + strings.Join(entries, ", ") + "]"
}

// FilterByAllowlist returns a new toolRegistry containing only the tools whose names are in the allowlist.
// Returns an error if the allowlist is empty or if any tool name in the allowlist does not exist in the registry.
func (r *toolRegistry) FilterByAllowlist(allowlist []string) (*toolRegistry, error) {
	if len(allowlist) == 0 {
		return nil, fmt.Errorf("allowlist must not be empty")
	}

	allowlistSet := make(map[string]struct{}, len(allowlist))
	var missing []string
	for _, name := range allowlist {
		allowlistSet[name] = struct{}{}
		if _, exists := r.configs[name]; !exists {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("tools in allowlist not found in registry: %s", strings.Join(missing, ", "))
	}

	filtered := newToolRegistry()

	for _, tool := range r.tools {
		if _, allowed := allowlistSet[tool.Name]; !allowed {
			continue
		}
		// config is guaranteed to exist: we validated all allowlist names above
		config := r.configs[tool.Name]
		if err := filtered.Register(tool, config); err != nil {
			zap.L().Warn("Unexpected error registering tool in filtered registry",
				zap.String("toolName", tool.Name), zap.Error(err))
		}
	}

	return filtered, nil
}
