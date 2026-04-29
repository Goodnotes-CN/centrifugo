package middleware

import (
	"net/http"

	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel/trace"
)

// TraceLogger extracts the active OTel SpanContext from the request context
// (populated by the OpenTelemetry HTTP middleware) and binds a zerolog logger
// with trace_id/span_id fields to the context. Downstream handlers that use
// log.Ctx(r.Context()) inherit these fields automatically, so log lines carry
// the upstream trace and the bridge can place them in the OTel record's
// first-class trace context slots.
//
// Must be placed AFTER OpenTelemetryHandler.Middleware in the chain.
func TraceLogger(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		sc := trace.SpanContextFromContext(ctx)
		if !sc.IsValid() {
			h.ServeHTTP(w, r)
			return
		}
		l := log.Logger.With().
			Str("trace_id", sc.TraceID().String()).
			Str("span_id", sc.SpanID().String()).
			Logger()
		h.ServeHTTP(w, r.WithContext(l.WithContext(ctx)))
	})
}
