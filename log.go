package envoy_mcp_openapi_processor

import (
	"context"
	"fmt"
	"os"

	"go.opentelemetry.io/contrib/bridges/otelzap"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// CreateConsoleCore returns a zapcore.Core that writes info/warn logs to stdout
// and error logs to stderr using a console encoder.
func CreateConsoleCore() zapcore.Core {
	return zapcore.NewTee(
		zapcore.NewCore(zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig()), zapcore.AddSync(os.Stdout),
			// log everything below error to stdout, and everything above to stderr stream.
			zap.LevelEnablerFunc(func(lvl zapcore.Level) bool {
				return lvl >= zapcore.InfoLevel && lvl <= zapcore.WarnLevel
			})),
		zapcore.NewCore(zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig()), zapcore.AddSync(os.Stderr), zapcore.ErrorLevel))
}

// CreateNewLoggerFromCore creates a named zap.Logger from the given core with caller information enabled.
func CreateNewLoggerFromCore(core zapcore.Core) *zap.Logger {
	return zap.New(core, zap.AddCaller()).Named(componentName)
}

// InitLogger sets up the global logger to use the OTel bridge, allowing logs to be exported to OTel.
func InitLogger(config TelemetryConfig) error {
	logger, err := createOtelLogger(config)
	if err != nil {
		return fmt.Errorf("failed to initialize logger: %w", err)
	}
	zap.ReplaceGlobals(logger)
	return nil
}

func createOtelLogger(config TelemetryConfig) (*zap.Logger, error) {
	core, err := createLoggerCore(config)
	if err != nil {
		return nil, err
	}
	return CreateNewLoggerFromCore(core), nil
}

func createLoggerCore(config TelemetryConfig) (zapcore.Core, error) {
	otelCore, err := createOtelCore(config)
	if err != nil {
		return nil, err
	}

	core := zapcore.NewTee(
		CreateConsoleCore(),
		otelCore)
	return core, nil
}

func newResource(config TelemetryConfig) (*resource.Resource, error) {
	return resource.Merge(resource.Default(),
		resource.NewWithAttributes(resource.Default().SchemaURL(),
			semconv.ServiceName(config.ServiceName),
		))
}

// createOtelCore constructs an OTel bridge to ship logs to OTel using a Zap logger.
func createOtelCore(config TelemetryConfig) (*otelzap.Core, error) {
	exporter, err := otlploggrpc.New(context.TODO(), otlploggrpc.WithEndpoint(config.OtelEndpoint), otlploggrpc.WithInsecure())
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP log exporter: %w", err)
	}
	otelResource, err := newResource(config)
	if err != nil {
		return nil, fmt.Errorf("cannot create OTel resource: %w", err)
	}
	processor := sdklog.NewBatchProcessor(exporter)
	provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(processor), sdklog.WithResource(otelResource))
	return otelzap.NewCore(componentName, otelzap.WithLoggerProvider(provider)), nil
}
