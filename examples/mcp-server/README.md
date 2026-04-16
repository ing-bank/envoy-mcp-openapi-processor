# MCP Server Example

This example shows how to expose a REST API as MCP tools using the `envoy-mcp-openapi-processor`.

It runs the external processor and Envoy as separate containers in front of a
[Prism](https://github.com/stoplightio/prism) mock backend serving the Petstore OpenAPI spec.
The [MCP Inspector](https://github.com/modelcontextprotocol/inspector) is included for interactive exploration.
Envoy and the external processor communicate over a Unix domain socket shared via a named Docker volume.

> **Warning:** The Envoy configuration in this example is **not safe for production use** and is intended **for demo
purposes only**.

## How it works

```
     MCP Inspector (:6274)
               │
               │  MCP over HTTP
               ▼
        Envoy (:10000)  ── ext_proc gRPC ──►  envoy-mcp-openapi-processor
               │               (Unix socket)          mutates MCP <-> REST
               │
               │  HTTP upstream
               ▼
   Prism mock server (:8080)
```

The external processor intercepts every request and response via Envoy's `ext_proc` filter. On the request path it
translates an incoming MCP tool call into a plain REST request; on the response path it translates the REST response
back into an MCP result.

## Usage

1. From this directory, run `docker compose up --build`. This will build and start the containers (
   `envoy-mcp-openapi-processor`, `envoy`, `prism`, and `inspector`).

2. To access the MCP Inspector UI:

    - Go to http://localhost:6274/?transport=streamable-http&serverUrl=http://localhost:10000/mcp
    - Select **Connection Type: Direct**
    - Click **Connect**

3. Once connected, you can:

    - **List Tools:** see the MCP tools generated from the Petstore OpenAPI spec.
    - **Call Tools:** invoke individual tools and inspect the responses.

4. To stop the example, press `Ctrl+C` in the terminal where docker compose was running.

## What's Inside

| File                 | Description                                                                                          |
|----------------------|------------------------------------------------------------------------------------------------------|
| `main.go`            | Entrypoint that runs the external processor (ext_proc) server.                                       |
| `envoy-mcp.yaml`     | Envoy configuration wiring the ext_proc filter and the Prism upstream.                               |
| `docker-compose.yml` | Compose file defining the `envoy-mcp-openapi-processor`, `envoy`, `prism`, and `inspector` services. |
| `Dockerfile`         | Multi-stage build: compiles the Go ext_proc binary into a minimal image.                             |