package envoy_mcp_openapi_processor

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/bridges/otelzap"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type testLogExporter struct {
	records []sdklog.Record
}

func newTestLogExporter() *testLogExporter {
	return &testLogExporter{
		records: make([]sdklog.Record, 0),
	}
}

func (e *testLogExporter) Export(ctx context.Context, records []sdklog.Record) error {
	e.records = append(e.records, records...)
	return nil
}

func (e *testLogExporter) Shutdown(ctx context.Context) error {
	return nil
}

func (e *testLogExporter) ForceFlush(ctx context.Context) error {
	return nil
}

func TestMcpServer_LogsToolCallsToOtel(t *testing.T) {
	exporter := newTestLogExporter()
	processor := sdklog.NewSimpleProcessor(exporter)
	provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(processor))
	defer func() { _ = provider.Shutdown(context.Background()) }()

	otelCore := otelzap.NewCore(componentName, otelzap.WithLoggerProvider(provider))
	logger := zap.New(zapcore.NewTee(CreateConsoleCore(zapcore.InfoLevel), otelCore), zap.AddCaller())
	restoreGlobals := zap.ReplaceGlobals(logger)
	defer restoreGlobals()

	registry, err := newToolRegistryFromConfig(&ToolRegistryConfig{OpenAPISpecPattern: "testdata/petstore.openapi.yaml"})
	require.NoError(t, err)
	server := &extProcServer{
		registry: registry,
	}

	// Make a tools/call request
	toolCallReq := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"listPets","arguments":{}}}`
	_ = server.mcpRequestHandler(context.Background(), []byte(toolCallReq), mcpRequestHeaders{})

	_ = logger.Sync()

	// Check that tool call was logged
	assert.NotEmpty(t, exporter.records, "Expected logs for tool call")
	t.Logf("Captured %d log records", len(exporter.records))

	// Log the captured records
	for i, record := range exporter.records {
		t.Logf("Log %d: %v", i, record.Body().AsString())
	}
}

func TestCreateConsoleCore_LevelGating(t *testing.T) {
	debugCore := CreateConsoleCore(zapcore.DebugLevel)
	assert.True(t, debugCore.Enabled(zapcore.DebugLevel))

	infoCore := CreateConsoleCore(zapcore.InfoLevel)
	assert.False(t, infoCore.Enabled(zapcore.DebugLevel))
	assert.True(t, infoCore.Enabled(zapcore.InfoLevel))
	assert.True(t, infoCore.Enabled(zapcore.ErrorLevel))
}

func TestInitLogger_InvalidLogLevel(t *testing.T) {
	err := InitLogger(TelemetryConfig{LogLevel: "verbose"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid config.LogLevel")
}
