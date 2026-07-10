package main

import (
	"context"
	"os"
	"strings"

	"go.opentelemetry.io/contrib/bridges/otelzap"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// logger is the process-wide logger. It starts console-only and is upgraded to
// also export to OpenTelemetry once telemetry is configured. baseConsole always
// logs only to the console, backing the SDK error handler so a failing
// collector can't be told about its own failures through the bridge.
var (
	logger      = buildLogger(nil)
	baseConsole = buildConsoleLogger()
)

// log returns the process logger carrying trace context from ctx: the active
// span's trace and span IDs are added as fields (so they show in the console),
// and the context itself is attached for the OpenTelemetry bridge to correlate
// the record with the span natively. Error context is best attached with
// humane.Zap(err), e.g. log(ctx).Warn("something failed", humane.Zap(err)...).
func log(ctx context.Context) *zap.Logger {
	if ctx == nil {
		return logger
	}
	fields := []zap.Field{ctxField(ctx)}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		fields = append(fields,
			zap.String("trace_id", sc.TraceID().String()),
			zap.String("span_id", sc.SpanID().String()),
		)
	}
	return logger.With(fields...)
}

// installLogger sets the process-wide logger. It is safe to call more than once
// (main installs a console-only logger before telemetry is configured).
func installLogger(lp otellog.LoggerProvider) {
	baseConsole = buildConsoleLogger()
	logger = buildLogger(lp)
}

// buildConsoleLogger builds a console-only logger at the configured level.
func buildConsoleLogger() *zap.Logger {
	lvl := zap.NewAtomicLevelAt(logLevel())
	return zap.New(zapcore.NewCore(consoleEncoder(), zapcore.Lock(os.Stderr), lvl))
}

// buildLogger constructs the process logger: a console core, teed with an
// OpenTelemetry bridge core when lp is non-nil so records also export via OTLP.
func buildLogger(lp otellog.LoggerProvider) *zap.Logger {
	lvl := zap.NewAtomicLevelAt(logLevel())
	cores := []zapcore.Core{zapcore.NewCore(consoleEncoder(), zapcore.Lock(os.Stderr), lvl)}
	if lp != nil {
		bridge := otelzap.NewCore(instrumentationName,
			otelzap.WithLoggerProvider(lp),
			otelzap.WithVersion(version),
		)
		cores = append(cores, leveledCore{Core: bridge, level: lvl})
	}
	return zap.New(zapcore.NewTee(cores...))
}

// consoleEncoder renders records in a compact, timestamped console form.
func consoleEncoder() zapcore.Encoder {
	return zapcore.NewConsoleEncoder(zapcore.EncoderConfig{
		TimeKey:        "time",
		LevelKey:       "level",
		MessageKey:     "msg",
		NameKey:        "logger",
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeTime:     zapcore.TimeEncoderOfLayout("2006/01/02 15:04:05"),
		EncodeLevel:    zapcore.LowercaseLevelEncoder,
		EncodeDuration: zapcore.StringDurationEncoder,
	})
}

// logLevel resolves the console log level from AUTOUPDATE_LOG_LEVEL.
func logLevel() zapcore.Level {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("AUTOUPDATE_LOG_LEVEL"))) {
	case "debug":
		return zapcore.DebugLevel
	case "warn", "warning":
		return zapcore.WarnLevel
	case "error":
		return zapcore.ErrorLevel
	default:
		return zapcore.InfoLevel
	}
}

// ctxField carries a context.Context to the OpenTelemetry bridge core (which
// reads it to correlate the record with the active span) while staying
// invisible to the console encoder.
func ctxField(ctx context.Context) zap.Field {
	return zap.Field{Type: zapcore.SkipType, Interface: ctx}
}

// leveledCore gates an embedded core (the OTel bridge) by a shared level so
// AUTOUPDATE_LOG_LEVEL governs both the console and the exported logs.
type leveledCore struct {
	zapcore.Core
	level zapcore.LevelEnabler
}

func (c leveledCore) Enabled(l zapcore.Level) bool {
	return c.level.Enabled(l) && c.Core.Enabled(l)
}

func (c leveledCore) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if !c.level.Enabled(e.Level) {
		return ce
	}
	return c.Core.Check(e, ce)
}

func (c leveledCore) With(fs []zapcore.Field) zapcore.Core {
	return leveledCore{Core: c.Core.With(fs), level: c.level}
}
