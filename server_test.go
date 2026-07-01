package envoy_mcp_openapi_processor

import (
	"context"
	"testing"
	"time"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestProcess_RecvCanceled(t *testing.T) {
	t.Parallel()

	// gRPC surfaces client cancellation as a Canceled status whose message
	// varies with the cause; the Process loop must treat any Canceled status as
	// a clean shutdown, not an error.
	requests := []*extProcPb.ProcessingRequest{
		requestHeadersMsg(false),
	}
	stream := &fakeProcessStream{
		requests: requests,
		recvErr:  status.Error(grpccodes.Canceled, "client canceled the stream"),
	}
	server := &extProcServer{allowedHosts: newHostAllowlist(nil)}
	require.NoError(t, server.Process(stream))
}

func TestRunServer_InvalidArguments(t *testing.T) {
	tests := []struct {
		name    string
		ctx     context.Context
		cfg     *Config
		wantErr string
	}{
		{
			name:    "nil context",
			ctx:     nil,
			cfg:     &Config{SocketPath: "/tmp/test.sock", ToolRegistryConfig: &ToolRegistryConfig{}},
			wantErr: "context must not be nil",
		},
		{
			name:    "nil config",
			ctx:     context.Background(),
			cfg:     nil,
			wantErr: "config must not be nil",
		},
		{
			name:    "nil tool registry config",
			ctx:     context.Background(),
			cfg:     &Config{SocketPath: "/tmp/test.sock"},
			wantErr: "config.ToolRegistryConfig must not be nil",
		},
		{
			name:    "empty socket path",
			ctx:     context.Background(),
			cfg:     &Config{ToolRegistryConfig: &ToolRegistryConfig{}},
			wantErr: "config.SocketPath must not be empty",
		},
		{
			name:    "empty allowed host entry",
			ctx:     context.Background(),
			cfg:     &Config{SocketPath: "/tmp/test.sock", ToolRegistryConfig: &ToolRegistryConfig{}, AllowedHosts: []string{""}},
			wantErr: `config.AllowedHosts entry "" must be a plain hostname or wildcard ("*", "*.example.com")`,
		},
		{
			name:    "allowed host with scheme",
			ctx:     context.Background(),
			cfg:     &Config{SocketPath: "/tmp/test.sock", ToolRegistryConfig: &ToolRegistryConfig{}, AllowedHosts: []string{"http://localhost"}},
			wantErr: `config.AllowedHosts entry "http://localhost" must be a plain hostname or wildcard ("*", "*.example.com")`,
		},
		{
			name:    "allowed host with bare wildcard label",
			ctx:     context.Background(),
			cfg:     &Config{SocketPath: "/tmp/test.sock", ToolRegistryConfig: &ToolRegistryConfig{}, AllowedHosts: []string{"*."}},
			wantErr: `config.AllowedHosts entry "*." must be a plain hostname or wildcard ("*", "*.example.com")`,
		},
		{
			name:    "allowed host with double wildcard",
			ctx:     context.Background(),
			cfg:     &Config{SocketPath: "/tmp/test.sock", ToolRegistryConfig: &ToolRegistryConfig{}, AllowedHosts: []string{"*.*"}},
			wantErr: `config.AllowedHosts entry "*.*" must be a plain hostname or wildcard ("*", "*.example.com")`,
		},
		{
			name:    "allowed host with embedded wildcard",
			ctx:     context.Background(),
			cfg:     &Config{SocketPath: "/tmp/test.sock", ToolRegistryConfig: &ToolRegistryConfig{}, AllowedHosts: []string{"a*b.com"}},
			wantErr: `config.AllowedHosts entry "a*b.com" must be a plain hostname or wildcard ("*", "*.example.com")`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := RunServer(tt.ctx, tt.cfg) //nolint:staticcheck // intentionally testing nil context rejection
			require.EqualError(t, err, tt.wantErr)
		})
	}
}

func TestConfig_Validate_AcceptsWildcards(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		SocketPath:         "/tmp/test.sock",
		ToolRegistryConfig: &ToolRegistryConfig{},
		AllowedHosts:       []string{"*", "*.example.com", "host.example.com", "localhost"},
	}
	require.NoError(t, cfg.validate())
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
