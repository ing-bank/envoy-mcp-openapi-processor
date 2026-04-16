FROM golang:1.25.7 AS builder
WORKDIR /src
# copy ext_proc deps info
COPY go.mod go.sum ./
# copy main deps info
COPY examples/mcp-server/go.mod examples/mcp-server/go.sum ./examples/mcp-server/
# download deps
WORKDIR /src/examples/mcp-server
RUN GOPROXY=direct go mod download
# copy all source code
WORKDIR /src
COPY . ./

WORKDIR /src/examples/mcp-server
RUN CGO_ENABLED=0 go build -o /envoy-mcp-openapi-processor

FROM debian:trixie-slim

RUN mkdir -p /var/run/shared && chown 10042:0 /var/run/shared
USER 10042
COPY --from=builder --chown=10042:0 /envoy-mcp-openapi-processor /usr/local/bin

ENTRYPOINT ["envoy-mcp-openapi-processor"]
