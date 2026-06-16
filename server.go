package envoy_mcp_openapi_processor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"strings"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type extProcServer struct {
	extProcPb.UnimplementedExternalProcessorServer
	registry     *toolRegistry
	serverInfo   ServerInfo
	allowedHosts *hostAllowlist
}

// if otel.SetTracerProvider(...) was not called by the library user, this will return a no-op tracer
var tracer = otel.Tracer(componentName)

func (s *extProcServer) Process(srv extProcPb.ExternalProcessor_ProcessServer) error {
	strm := &stream{
		server: s,
		buf:    make([]byte, 0, initialBodyBufCap),
	}
	var state streamState = &stateAwaitingRequestHeaders{}

	ctx := srv.Context()
	ctx, processSpan := tracer.Start(ctx, "ext_proc.Process",
		trace.WithSpanKind(trace.SpanKindServer),
	)
	defer processSpan.End()

	fail := func(err error, msg string) error {
		processSpan.RecordError(err)
		processSpan.SetStatus(codes.Error, msg)
		return status.Errorf(grpccodes.Unknown, "%s: %v", msg, err)
	}

	for {
		req, err := srv.Recv()
		switch {
		case errors.Is(err, io.EOF) || status.Code(err) == grpccodes.Canceled:
			return nil
		case err != nil:
			return fail(err, "error receiving request")
		}

		next, reps, err := state.handle(ctx, strm, req)
		if err != nil {
			processSpan.RecordError(err)
			processSpan.SetStatus(codes.Error, err.Error())
			return err
		}
		state = next

		for _, rep := range reps {
			if err := srv.Send(rep); err != nil {
				return fail(err, "error sending response")
			}
		}
	}
}

// RunServer starts the ext_proc gRPC server on a Unix domain socket, loading
// MCP tools from the OpenAPI specs specified in cfg and serving Envoy external
// processing requests until ctx is cancelled.
func RunServer(ctx context.Context, cfg *Config) error {
	if ctx == nil {
		return errors.New("context must not be nil")
	}
	if cfg == nil {
		return errors.New("config must not be nil")
	}
	if err := cfg.validate(); err != nil {
		return err
	}

	// Load tools from OpenAPI spec
	registry, err := newToolRegistryFromConfig(cfg.ToolRegistryConfig)
	if err != nil {
		return fmt.Errorf("failed to load tools config: %w", err)
	}

	// Apply allowlist filtering if configured
	if len(cfg.ToolRegistryConfig.ToolAllowlist) > 0 {
		zap.L().Info("Tools before filtering", zap.String("tools", registry.String()), zap.Strings("allowlist", cfg.ToolRegistryConfig.ToolAllowlist))
		registry, err = registry.FilterByAllowlist(cfg.ToolRegistryConfig.ToolAllowlist)
		if err != nil {
			return fmt.Errorf("failed to apply allowlist filter: %w", err)
		}
		zap.L().Info("Tools after filtering", zap.String("tools", registry.String()))
	}

	if err := os.Remove(cfg.SocketPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	lis, err := net.ListenUnix("unix", &net.UnixAddr{
		Name: cfg.SocketPath,
		Net:  "unix",
	})
	if err != nil {
		return err
	}

	defer func() {
		_ = lis.Close()
	}()

	// Create gRPC server with OpenTelemetry instrumentation
	// The StatsHandler automatically creates spans for all gRPC calls
	grpcServer := grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler(
			otelgrpc.WithTracerProvider(otel.GetTracerProvider()),
			otelgrpc.WithMessageEvents(otelgrpc.ReceivedEvents, otelgrpc.SentEvents),
		)),
	)

	allowedHosts := cfg.AllowedHosts
	if len(allowedHosts) == 0 {
		allowedHosts = defaultAllowedHosts
	}
	zap.L().Info("Host/Origin validation (DNS rebinding protection) enabled", zap.Strings("allowedHosts", allowedHosts))

	extProcPb.RegisterExternalProcessorServer(grpcServer, &extProcServer{
		registry:     registry,
		serverInfo:   cfg.ServerInfo,
		allowedHosts: newHostAllowlist(allowedHosts),
	})

	go func() {
		<-ctx.Done()
		zap.L().Info("Shutting down ext_proc server...")
		grpcServer.GracefulStop()
	}()

	zap.L().Info("Starting ext_proc server", zap.String("address", lis.Addr().String()))
	return grpcServer.Serve(lis)
}

// ServerInfo holds the identity and instructions returned in the MCP initialize response.
type ServerInfo struct {
	// Name is the server name reported to MCP clients.
	Name string
	// Version is the server version reported to MCP clients.
	Version string
	// Instructions is human-readable text returned to MCP clients.
	Instructions string
}

// Config holds the configuration for the ext_proc gRPC server.
type Config struct {
	// SocketPath is the Unix domain socket path the gRPC server listens on.
	SocketPath string
	// ToolRegistryConfig configures how tools are loaded from OpenAPI specs.
	ToolRegistryConfig *ToolRegistryConfig
	// ServerInfo configures the identity and instructions for the MCP server.
	ServerInfo ServerInfo
	// AllowedHosts is the list of hostnames accepted in the Host/:authority and
	// Origin headers of incoming requests (DNS rebinding protection). Matching
	// is case-insensitive and ignores ports. Each entry is one of:
	//   - "*" — match any host (disables rebinding protection; opt-in only);
	//   - "*.example.org" — match any subdomain at any depth (foo.example.org,
	//     a.b.example.org) but NOT the apex example.org;
	//   - "host.example.org" — exact match.
	// Empty means the default policy: localhost, 127.0.0.1 and ::1. A non-empty
	// list REPLACES the default — include "localhost" explicitly if it should
	// remain allowed.
	AllowedHosts []string
}

func (c *Config) validate() error {
	if c.ToolRegistryConfig == nil {
		return errors.New("config.ToolRegistryConfig must not be nil")
	}
	if c.SocketPath == "" {
		return errors.New("config.SocketPath must not be empty")
	}
	for _, h := range c.AllowedHosts {
		if !validAllowedHost(h) {
			return fmt.Errorf(`config.AllowedHosts entry %q must be a plain hostname or wildcard ("*", "*.example.com")`, h)
		}
	}
	return nil
}

// validAllowedHost reports whether an AllowedHosts entry is well-formed: the
// bare "*", a "*.host" subdomain wildcard, or a plain hostname. Schemes, paths,
// whitespace and malformed wildcards (e.g. "*.", "*.*", "a*b.com") are rejected
// so misconfiguration fails loudly rather than silently allowing nothing.
func validAllowedHost(h string) bool {
	trimmed := strings.TrimSpace(h)
	if trimmed == "" || strings.ContainsAny(trimmed, "/ \t") {
		return false
	}
	if trimmed == "*" {
		return true
	}
	if rest, ok := strings.CutPrefix(trimmed, "*."); ok {
		// The remainder must be a plain hostname with no further wildcards.
		return rest != "" && !strings.Contains(rest, "*")
	}
	return !strings.Contains(trimmed, "*")
}
