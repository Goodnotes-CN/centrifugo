package telemetry

// This file holds the GoodNotes APMPlus additions on top of upstream's
// tracing-only provider.go. It is intentionally additive: it does not modify
// provider.go (which keeps upstream's tracing setup, including the optional
// Google Cloud ADC auth) and instead layers metrics + logs export and lifecycle
// management around it.

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
//
// Tracing is delegated to upstream's SetupTracing (so the Google Cloud ADC auth
// option keeps working); metrics and logs are GoodNotes APMPlus additions.
func Setup(ctx context.Context, enableMetrics, enableLogs, googleCloudADCAuth bool) (*Providers, error) {
	p := &Providers{}

	tp, err := SetupTracing(ctx, googleCloudADCAuth)
	if err != nil {
		return nil, fmt.Errorf("setup tracing: %w", err)
	}
	p.tracer = tp

	// Upstream SetupTracing installs a TraceContext-only propagator. Extend it
	// with Baggage so otelhttp/otelgrpc honour upstream traceparent headers and
	// forward our span context + baggage to downstream services on both inbound
	// and outbound calls.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

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
// defaulting to "http/protobuf" when unset. Shared by metrics and logs.
func otlpProtocol() string {
	p := os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	if p == "" {
		return "http/protobuf"
	}
	return p
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

// otelLoggerName is the instrumentation scope used by the zerolog bridge.
const otelLoggerName = "centrifugo"

// otelLogger returns the OTel logger used by the zerolog bridge.
func otelLogger(lp otellog.LoggerProvider) otellog.Logger {
	return lp.Logger(otelLoggerName)
}
