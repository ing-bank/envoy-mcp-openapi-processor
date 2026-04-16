package envoy_mcp_openapi_processor

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// InitTracer initializes the OpenTelemetry tracer and returns a shutdown function
func InitTracer(ctx context.Context, config TelemetryConfig) (func(context.Context) error, error) {
	// Create OTLP trace exporter that sends to OpenTelemetry Collector
	conn, err := grpc.NewClient(config.OtelEndpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create gRPC connection to collector: %w", err)
	}

	traceExporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithGRPCConn(conn),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create trace exporter: %w", err)
	}

	// Create resource with service information
	res, err := resource.Merge(
		resource.Default(),
		resource.NewSchemaless(
			attribute.String("service.name", config.ServiceName),
			attribute.String("service.version", config.ServiceVersion),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	// Create trace provider with the exporter
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExporter,
			sdktrace.WithBatchTimeout(time.Second),
		),
		sdktrace.WithResource(res),
		// Sample all traces - adjust for production
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)

	// Set the global tracer provider
	otel.SetTracerProvider(tracerProvider)

	// Set the global propagator to W3C Trace Context (this is what carries traceparent)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return tracerProvider.Shutdown, nil
}
