package envoy_mcp_openapi_processor

// TelemetryConfig holds the configuration for OpenTelemetry tracing and logging exporters.
type TelemetryConfig struct {
	// OtelEndpoint is the gRPC address of the OpenTelemetry Collector (e.g. "localhost:4317").
	OtelEndpoint string
	// ServiceName identifies this service in traces and logs.
	ServiceName string
	// ServiceVersion is the version string reported in telemetry resource attributes.
	ServiceVersion string
}
