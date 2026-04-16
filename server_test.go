package envoy_mcp_openapi_processor

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunServer_NilContext(t *testing.T) {
	cfg := &Config{
		SocketPath:         "/tmp/test.sock",
		ToolRegistryConfig: &ToolRegistryConfig{},
	}
	err := RunServer(nil, cfg) //nolint:staticcheck // intentionally testing nil context rejection
	require.EqualError(t, err, "context must not be nil")
}

func TestRunServer_NilConfig(t *testing.T) {
	err := RunServer(context.Background(), nil)
	require.EqualError(t, err, "config must not be nil")
}

func TestRunServer_NilToolRegistryConfig(t *testing.T) {
	cfg := &Config{
		SocketPath: "/tmp/test.sock",
	}
	err := RunServer(context.Background(), cfg)
	require.EqualError(t, err, "config.ToolRegistryConfig must not be nil")
}

func TestRunServer_EmptySocketPath(t *testing.T) {
	cfg := &Config{
		ToolRegistryConfig: &ToolRegistryConfig{},
	}
	err := RunServer(context.Background(), cfg)
	require.EqualError(t, err, "config.SocketPath must not be empty")
}

func TestRunServer_FailsWhenNoToolsLoaded(t *testing.T) {
	cfg := &Config{
		SocketPath:         t.TempDir() + "/test.sock",
		ToolRegistryConfig: &ToolRegistryConfig{},
	}

	err := RunServer(t.Context(), cfg)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to load tools config")
	assert.Contains(t, err.Error(), "no files matched pattern")
}

func TestRunServer_SucceedsWithValidTools(t *testing.T) {
	cfg := &Config{
		SocketPath: t.TempDir() + "/test.sock",
		ToolRegistryConfig: &ToolRegistryConfig{
			OpenAPISpecPattern: "testdata/minimal-users.openapi.yaml",
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- RunServer(ctx, cfg)
	}()

	select {
	case err := <-errCh:
		if err != nil {
			assert.NotContains(t, err.Error(), "failed to load tools config",
				"Server should not fail due to tool loading when valid specs are provided")
		}
	case <-time.After(time.Second):
		t.Fatal("Test timed out waiting for server to stop")
	}
}
