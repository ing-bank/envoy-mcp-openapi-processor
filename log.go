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

// CreateConsoleCore returns a zapcore.Core that writes logs at or above level
// to stdout and error logs to stderr using a console encoder.
func CreateConsoleCore(level zapcore.Level) zapcore.Core {
	return zapcore.NewTee(
		zapcore.NewCore(zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig()), zapcore.AddSync(os.Stdout),
			// log everything below error to stdout, and everything above to stderr stream.
			zap.LevelEnablerFunc(func(l zapcore.Level) bool {
				return l >= level && l < zapcore.ErrorLevel
			})),
		zapcore.NewCore(zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig()), zapcore.AddSync(os.Stderr), zapcore.ErrorLevel))
}

// CreateNewLoggerFromCore creates a named zap.Logger from the given core with caller information enabled.
func CreateNewLoggerFromCore(core zapcore.Core) *zap.Logger {
	return zap.New(core, zap.AddCaller()).Named(componentName)
}

// InitLogger sets up the global logger to use the OTel bridge, allowing logs to be exported to OTel.
// Logs below config.LogLevel are written neither to the console nor to OTel.
func InitLogger(config TelemetryConfig) error {
	level, err := zapcore.ParseLevel(config.LogLevel)
	if err != nil {
		return fmt.Errorf("invalid config.LogLevel %q: %w", config.LogLevel, err)
	}
	logger, err := createOtelLogger(config, level)
	if err != nil {
		return fmt.Errorf("failed to initialize logger: %w", err)
	}
	zap.ReplaceGlobals(logger)
	return nil
}

func createOtelLogger(config TelemetryConfig, level zapcore.Level) (*zap.Logger, error) {
	core, err := createLoggerCore(config, level)
	if err != nil {
		return nil, err
	}
	return CreateNewLoggerFromCore(core), nil
}

func createLoggerCore(config TelemetryConfig, level zapcore.Level) (zapcore.Core, error) {
	otelCore, err := createOtelCore(config)
	if err != nil {
		return nil, err
	}

	// otelzap has no level option and its Enabled delegates to the OTel SDK,
	// which accepts every severity. Gate it so OTel matches the console.
	gatedOtelCore, err := zapcore.NewIncreaseLevelCore(otelCore, level)
	if err != nil {
		return nil, fmt.Errorf("cannot apply log level to OTel core: %w", err)
	}

	core := zapcore.NewTee(
		CreateConsoleCore(level),
		gatedOtelCore)
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
