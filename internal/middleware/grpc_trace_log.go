package middleware

import (
	"context"

	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
)

// GRPCTraceLoggerUnary is the gRPC unary-server counterpart of TraceLogger:
// it extracts the active SpanContext (populated by otelgrpc StatsHandler) and
// binds a zerolog logger with trace_id/span_id to the ctx so downstream
// handlers using log.Ctx(ctx) inherit those fields and the bridge places them
// in the OTel log record's first-class trace context slots.
//
// Must be installed AFTER otelgrpc.NewServerHandler in the interceptor chain.
func GRPCTraceLoggerUnary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		return handler(traceCtx(ctx), req)
	}
}

// GRPCTraceLoggerStream is the streaming counterpart of GRPCTraceLoggerUnary.
func GRPCTraceLoggerStream() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		wrapped := &wrappedServerStream{ServerStream: ss, ctx: traceCtx(ss.Context())}
		return handler(srv, wrapped)
	}
}

func traceCtx(ctx context.Context) context.Context {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ctx
	}
	l := log.Logger.With().
		Str("trace_id", sc.TraceID().String()).
		Str("span_id", sc.SpanID().String()).
		Logger()
	return l.WithContext(ctx)
}

type wrappedServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedServerStream) Context() context.Context { return w.ctx }
