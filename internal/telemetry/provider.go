package telemetry

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/centrifugal/centrifugo/v6/internal/build"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog/log"
	prombridge "go.opentelemetry.io/contrib/bridges/prometheus"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.20.0"
)

// Providers holds all three OTel providers and manages their lifecycle.
type Providers struct {
	tracer *sdktrace.TracerProvider
	meter  *sdkmetric.MeterProvider
	logger *sdklog.LoggerProvider
}

// Shutdown flushes and shuts down all providers gracefully.
func (p *Providers) Shutdown(ctx context.Context) {
	if p.tracer != nil {
		if err := p.tracer.Shutdown(ctx); err != nil {
			log.Err(err).Msg("error shutting down tracer provider")
		}
	}
	if p.meter != nil {
		if err := p.meter.Shutdown(ctx); err != nil {
			log.Err(err).Msg("error shutting down meter provider")
		}
	}
	if p.logger != nil {
		if err := p.logger.Shutdown(ctx); err != nil {
			log.Err(err).Msg("error shutting down logger provider")
		}
	}
}

// Setup initialises all enabled OTel providers (traces, metrics, logs) and
// registers them as the global providers. Call Providers.Shutdown on exit.
func Setup(ctx context.Context, enableMetrics, enableLogs bool) (*Providers, error) {
	p := &Providers{}

	tp, err := setupTracing(ctx)
	if err != nil {
		return nil, fmt.Errorf("setup tracing: %w", err)
	}
	p.tracer = tp

	if enableMetrics {
		mp, err := setupMetrics(ctx)
		if err != nil {
			return nil, fmt.Errorf("setup metrics: %w", err)
		}
		p.meter = mp
	}

	if enableLogs {
		lp, err := setupLogging(ctx)
		if err != nil {
			return nil, fmt.Errorf("setup logging: %w", err)
		}
		p.logger = lp
	}

	return p, nil
}

// SetupTracing is kept for backward compatibility; prefer Setup.
func SetupTracing(ctx context.Context) (*sdktrace.TracerProvider, error) {
	return setupTracing(ctx)
}

func newResource() (*resource.Resource, error) {
	serviceName := os.Getenv("OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = "centrifugo"
	}
	return resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceNameKey.String(serviceName),
		attribute.String("version", build.Version),
	), nil
}

// ── Traces ────────────────────────────────────────────────────────────────────

func setupTracing(ctx context.Context) (*sdktrace.TracerProvider, error) {
	exporterProtocol := os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	if exporterProtocol == "" {
		exporterProtocol = "http/protobuf"
	}

	exporter, err := createTraceExporter(ctx, exporterProtocol)
	if err != nil {
		return nil, err
	}

	rs, err := newResource()
	if err != nil {
		return nil, err
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(rs),
	)

	otel.SetTracerProvider(provider)
	otel.SetErrorHandler(&errorHandlerImpl{})

	return provider, nil
}

func createTraceExporter(ctx context.Context, exporterProtocol string) (*otlptrace.Exporter, error) {
	if exporterProtocol == "grpc" {
		return otlptracegrpc.New(ctx)
	}
	if exporterProtocol == "http/protobuf" {
		return otlptracehttp.New(ctx)
	}
	return nil, fmt.Errorf("unsupported exporter protocol: %s", exporterProtocol)
}

// ── Metrics ───────────────────────────────────────────────────────────────────

func setupMetrics(ctx context.Context) (*sdkmetric.MeterProvider, error) {
	exporter, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, err
	}

	rs, err := newResource()
	if err != nil {
		return nil, err
	}

	reader := sdkmetric.NewPeriodicReader(
		exporter,
		// Go runtime metrics (goroutines, GC, memory histograms, …)
		sdkmetric.WithProducer(runtime.NewProducer()),
		// Existing Prometheus metrics (centrifugo_* + centrifuge_* + go_*)
		sdkmetric.WithProducer(prombridge.NewMetricProducer(
			prombridge.WithGatherer(prometheus.DefaultGatherer),
		)),
	)

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(rs),
	)
	otel.SetMeterProvider(mp)

	if err := runtime.Start(
		runtime.WithMinimumReadMemStatsInterval(time.Second),
	); err != nil {
		return nil, fmt.Errorf("start runtime metrics: %w", err)
	}

	return mp, nil
}

// ── Logs ──────────────────────────────────────────────────────────────────────

func setupLogging(ctx context.Context) (*sdklog.LoggerProvider, error) {
	exporter, err := otlploghttp.New(ctx)
	if err != nil {
		return nil, err
	}

	rs, err := newResource()
	if err != nil {
		return nil, err
	}

	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter)),
		sdklog.WithResource(rs),
	)
	global.SetLoggerProvider(lp)

	// Wire zerolog → OTel log bridge.
	installZerologBridge(lp)

	return lp, nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

type errorHandlerImpl struct{}

func (e errorHandlerImpl) Handle(err error) {
	log.Err(err).Msg("opentelemetry error")
}

// otelLoggerName is the instrumentation scope used by the zerolog bridge.
const otelLoggerName = "centrifugo"

// logger returns the OTel logger used by the zerolog bridge.
func otelLogger(lp otellog.LoggerProvider) otellog.Logger {
	return lp.Logger(otelLoggerName)
}
