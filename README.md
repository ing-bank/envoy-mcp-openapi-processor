# envoy-mcp-openapi-processor

An [Envoy external processor](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/ext_proc_filter) 
that transforms [Model Context Protocol (MCP)](https://modelcontextprotocol.io/) requests into upstream HTTP API calls 
based on [OpenAPI](https://www.openapis.org/) specifications.


The external processor server communicates only with Envoy over gRPC via a Unix domain socket. Envoy owns all 
downstream client and upstream service connections, while the processor inspects and can mutate request and response 
data relayed by Envoy.

## Installation

```sh
go get github.com/ing-bank/envoy-mcp-openapi-processor
```

## Quick Start

See [`examples/mcp-server`](examples/mcp-server) for a complete working example including Envoy proxy configured to use the
`envoy-mcp-openapi-processor` server.

## OpenTelemetry

To enable export of logs and traces to an OpenTelemetry collector, use one of the options below. If neither is used, the server runs with a no-op tracer provider and a no-op logger.

### Option 1: Use provided initialization functions

```go
package main

import (
	"context"

	mcp_proc "github.com/ing-bank/envoy-mcp-openapi-processor"
)

func main() {
	telemetryConfig := mcp_proc.TelemetryConfig{
		OtelEndpoint:   "otel-collector:4317",
		ServiceName:    "mcp-sidecar",
		ServiceVersion: "1.0.0",
	}
	ctx := context.Background()
	err := mcp_proc.InitLogger(telemetryConfig)
	// ...
	tracerShutdown, err := mcp_proc.InitTracer(ctx, telemetryConfig)
	// ...
	var cfg mcp_proc.Config
	// ...
	mcp_proc.RunServer(ctx, &cfg)
}
```

### Option 2: Bring your own provider

Set the global tracer provider by calling `otel.SetTracerProvider(myTracerProvider)` and the global zap logger by calling `zap.ReplaceGlobals(myLogger)` before starting the server.

## Development

### Tests

To run all tests, execute the following command:

```sh
make test
```

To check the test coverage, execute the following command:

```sh
make coverage-check
```

### Fuzz tests

Run server handler fuzz tests one at a time:

```sh
make fuzz-request
```

```sh
make fuzz-response
```

Each command runs until stopped with `Ctrl+C` and reports any issues found.


## Security Checklist

Before deploying to production, we advise to verify if the following controls are in place

### Container Security

- [ ] Container runs as non-root user
- [ ] Read-only root filesystem where possible
- [ ] No privileged mode
- [ ] Resource limits (CPU, memory) configured
- [ ] seccomp profile applied
- [ ] AppArmor/SELinux enabled
- [ ] Container image scanned for CVEs
- [ ] Image signed and verified

### Envoy Configuration

- [ ] TLS configured for downstream connections
- [ ] TLS configured for upstream connections
- [ ] Buffer limits set (≤32KB for edge deployments)
- [ ] Connection limits configured
- [ ] Stream limits configured (≤100 concurrent for HTTP/2)
- [ ] Timeouts configured (connection, stream, request)
- [ ] Overload manager enabled
- [ ] Admin endpoint restricted to localhost
- [ ] `use_remote_address: true` for edge deployments
- [ ] Path normalization enabled
- [ ] `headers_with_underscores_action: REJECT_REQUEST`