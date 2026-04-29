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
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
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
	// Propagate W3C TraceContext + Baggage on inbound and outbound calls so
	// otelhttp/otelgrpc honour upstream traceparent headers and forward our
	// span context to downstream services. Without this the SDK uses a NoOp
	// propagator and every request becomes a new root trace.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

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

// otlpProtocol returns the OTLP exporter protocol from OTEL_EXPORTER_OTLP_PROTOCOL,
// defaulting to "http/protobuf" when unset. Shared by traces, metrics and logs.
func otlpProtocol() string {
	p := os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	if p == "" {
		return "http/protobuf"
	}
	return p
}

// ── Traces ────────────────────────────────────────────────────────────────────

func setupTracing(ctx context.Context) (*sdktrace.TracerProvider, error) {
	exporter, err := createTraceExporter(ctx, otlpProtocol())
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

func createTraceExporter(ctx context.Context, protocol string) (*otlptrace.Exporter, error) {
	switch protocol {
	case "grpc":
		return otlptracegrpc.New(ctx)
	case "http/protobuf":
		return otlptracehttp.New(ctx)
	}
	return nil, fmt.Errorf("unsupported exporter protocol: %s", protocol)
}

// ── Metrics ───────────────────────────────────────────────────────────────────

func setupMetrics(ctx context.Context) (*sdkmetric.MeterProvider, error) {
	exporter, err := createMetricExporter(ctx, otlpProtocol())
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

func createMetricExporter(ctx context.Context, protocol string) (sdkmetric.Exporter, error) {
	switch protocol {
	case "grpc":
		return otlpmetricgrpc.New(ctx)
	case "http/protobuf":
		return otlpmetrichttp.New(ctx)
	}
	return nil, fmt.Errorf("unsupported exporter protocol: %s", protocol)
}

// ── Logs ──────────────────────────────────────────────────────────────────────

func setupLogging(ctx context.Context) (*sdklog.LoggerProvider, error) {
	exporter, err := createLogExporter(ctx, otlpProtocol())
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

func createLogExporter(ctx context.Context, protocol string) (sdklog.Exporter, error) {
	switch protocol {
	case "grpc":
		return otlploggrpc.New(ctx)
	case "http/protobuf":
		return otlploghttp.New(ctx)
	}
	return nil, fmt.Errorf("unsupported exporter protocol: %s", protocol)
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
