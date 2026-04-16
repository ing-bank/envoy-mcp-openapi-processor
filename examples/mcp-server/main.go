package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	mcp_proc "github.com/ing-bank/envoy-mcp-openapi-processor"
	"go.uber.org/zap"
)

func main() {
	ctx := context.Background()
	zap.ReplaceGlobals(mcp_proc.CreateNewLoggerFromCore(mcp_proc.CreateConsoleCore()))

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	setUpSignalHandler(cancel)

	cfg := &mcp_proc.Config{
		SocketPath: "/var/run/shared/ext_proc.sock",
		ToolRegistryConfig: &mcp_proc.ToolRegistryConfig{
			OpenAPISpecPattern: "/home/app/conf/openapi/*.yaml",
			StructuredOutput:   true,
		},
		ServerInfo: mcp_proc.ServerInfo{
			Name:         "mcp-server",
			Version:      "1.0.0",
			Instructions: "OpenAPI MCP gateway",
		},
	}

	if err := mcp_proc.RunServer(ctx, cfg); err != nil {
		zap.L().Error("Server error", zap.Error(err))
		os.Exit(1)
	}
}

func setUpSignalHandler(cancelFn context.CancelFunc) {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	go func() {
		sig := <-sigChan
		zap.L().Info("Received signal, shutting down...", zap.String("signal", sig.String()))
		cancelFn()
	}()
}
