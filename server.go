package envoy_mcp_openapi_processor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type extProcServer struct {
	extProcPb.UnimplementedExternalProcessorServer
	registry   *toolRegistry
	serverInfo ServerInfo
}

const (
	kb = 1024
	mb = 1024 * kb
)

// if otel.SetTracerProvider(...) was not called by the library user, this will return a no-op tracer
var tracer = otel.Tracer(componentName)

func (s *extProcServer) Process(srv extProcPb.ExternalProcessor_ProcessServer) error {

	var jsonrpcID jsonrpc.ID
	var currentToolRepConfig *toolResponseConfig
	var currentUpstreamStatusCode int

	// Extract trace context from the incoming gRPC stream context
	ctx := srv.Context()

	// Create a root span for the entire Process stream
	ctx, processSpan := tracer.Start(ctx, "ext_proc.Process",
		trace.WithSpanKind(trace.SpanKindServer),
	)
	defer processSpan.End()

	for {
		req, err := srv.Recv()
		switch {
		case errors.Is(err, io.EOF), errors.Is(err, status.Error(grpccodes.Canceled, context.Canceled.Error())):
			return nil
		case err != nil:
			processSpan.RecordError(err)
			processSpan.SetStatus(codes.Error, "error receiving request")
			return status.Errorf(grpccodes.Unknown, "error receiving request: %v", err)
		}

		var resp *extProcPb.ProcessingResponse

		switch value := req.Request.(type) {
		case *extProcPb.ProcessingRequest_RequestHeaders:
			_, span := tracer.Start(ctx, "ext_proc.request_headers",
				trace.WithSpanKind(trace.SpanKindInternal),
			)

			resp = &extProcPb.ProcessingResponse{
				Response: &extProcPb.ProcessingResponse_RequestHeaders{
					RequestHeaders: &extProcPb.HeadersResponse{
						Response: &extProcPb.CommonResponse{},
					},
				},
			}
			span.End()

		case *extProcPb.ProcessingRequest_RequestBody:
			ctx, span := tracer.Start(ctx, "ext_proc.request_body",
				trace.WithSpanKind(trace.SpanKindInternal),
			)
			span.SetAttributes(
				attribute.Int(attrBodySize, len(value.RequestBody.GetBody())),
			)

			handlerResult := s.mcpRequestHandler(ctx, value.RequestBody.GetBody())
			jsonrpcID = handlerResult.Id
			currentUpstreamStatusCode = 0
			resp = handlerResult.ProcRep
			if handlerResult.ToolConfigRef != nil {
				currentToolRepConfig = &handlerResult.ToolConfigRef.toolResponseConfig
			}
			span.End()

		case *extProcPb.ProcessingRequest_ResponseHeaders:
			// processing of response headers is needed to prevent envoy from
			// returning empty body to the client in case of upstreams response
			// without a body (e.g., 204 No Content)
			_, span := tracer.Start(ctx, "ext_proc.response_headers",
				trace.WithSpanKind(trace.SpanKindInternal),
			)

			if statusCode, ok := parseUpstreamStatusCode(value.ResponseHeaders.GetHeaders()); ok {
				currentUpstreamStatusCode = statusCode
			}

			if value.ResponseHeaders.GetEndOfStream() {
				resp = buildEndOfStreamResponse(ctx, jsonrpcID, currentUpstreamStatusCode, currentToolRepConfig)
			} else {
				resp = &extProcPb.ProcessingResponse{
					Response: &extProcPb.ProcessingResponse_ResponseHeaders{
						ResponseHeaders: &extProcPb.HeadersResponse{
							Response: &extProcPb.CommonResponse{},
						},
					},
				}
			}

			span.End()
		case *extProcPb.ProcessingRequest_ResponseBody:
			ctx, span := tracer.Start(ctx, "ext_proc.response_body",
				trace.WithSpanKind(trace.SpanKindInternal),
			)
			span.SetAttributes(attribute.Int(attrBodySize, len(value.ResponseBody.GetBody())))
			span.SetAttributes(attribute.Bool(attrResponseToolCallContextPresent, currentToolRepConfig != nil))
			span.SetAttributes(attribute.Bool(attrResponseJSONRPCIDPresent, jsonrpcID.IsValid()))
			if jsonrpcID.IsValid() {
				span.SetAttributes(
					attribute.String(attrJSONRPCID, fmt.Sprint(jsonrpcID)),
				)
			}
			resp = s.mcpResponseHandler(ctx, value.ResponseBody.GetBody(), jsonrpcID, currentToolRepConfig, httpStatusToErrorInfo(currentUpstreamStatusCode))
			span.End()

		default:
			continue
		}

		if err := srv.Send(resp); err != nil {
			processSpan.RecordError(err)
			processSpan.SetStatus(codes.Error, "error sending response")
			return status.Errorf(grpccodes.Unknown, "error sending response: %v", err)
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

	if _, err := os.Stat(cfg.SocketPath); err == nil {
		if err := os.Remove(cfg.SocketPath); err != nil {
			return err
		}
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
		grpc.MaxRecvMsgSize(100*mb),
		grpc.MaxSendMsgSize(100*mb),
	)

	extProcPb.RegisterExternalProcessorServer(grpcServer, &extProcServer{
		registry:   registry,
		serverInfo: cfg.ServerInfo,
	})

	go func() {
		<-ctx.Done()
		zap.L().Info("Shutting down ext_proc server...")
		grpcServer.GracefulStop()
	}()

	zap.L().Info("Starting ext_proc server", zap.String("address", lis.Addr().String()))
	if err := grpcServer.Serve(lis); err != nil {
		return err
	}
	return nil
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
}

func (c *Config) validate() error {
	if c.ToolRegistryConfig == nil {
		return errors.New("config.ToolRegistryConfig must not be nil")
	}
	if c.SocketPath == "" {
		return errors.New("config.SocketPath must not be empty")
	}
	return nil
}
