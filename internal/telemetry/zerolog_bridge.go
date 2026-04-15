package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	otellog "go.opentelemetry.io/otel/log"
)

// installZerologBridge replaces the global zerolog writer with a MultiLevelWriter
// that writes to stderr (unchanged) and forwards each log record to the OTel
// LoggerProvider.  Must be called after zerolog is fully configured.
func installZerologBridge(lp otellog.LoggerProvider) {
	bridge := &zerologOTelWriter{logger: otelLogger(lp)}
	log.Logger = log.Logger.Output(zerolog.MultiLevelWriter(os.Stderr, bridge))
}

// zerologOTelWriter implements zerolog.LevelWriter.  It parses each JSON line
// produced by zerolog and emits a corresponding OTel log record.
type zerologOTelWriter struct {
	logger otellog.Logger
}

// WriteLevel is called by zerolog for every log event.
func (w *zerologOTelWriter) WriteLevel(level zerolog.Level, p []byte) (int, error) {
	w.emit(level, p)
	return len(p), nil
}

// Write handles log lines that arrive without an explicit level (rare).
func (w *zerologOTelWriter) Write(p []byte) (int, error) {
	w.emit(zerolog.NoLevel, p)
	return len(p), nil
}

// emit converts a zerolog JSON line to an OTel log.Record and emits it.
func (w *zerologOTelWriter) emit(level zerolog.Level, p []byte) {
	var fields map[string]interface{}
	if err := json.Unmarshal(p, &fields); err != nil {
		// Malformed line – forward as a plain string body.
		var r otellog.Record
		r.SetTimestamp(time.Now())
		r.SetSeverity(zerologLevelToOTel(level))
		r.SetBody(otellog.StringValue(string(p)))
		w.logger.Emit(context.Background(), r)
		return
	}

	var r otellog.Record

	// Timestamp.
	if ts, ok := parseTime(fields[zerolog.TimestampFieldName]); ok {
		r.SetTimestamp(ts)
	} else {
		r.SetTimestamp(time.Now())
	}

	// Severity – prefer the level embedded in JSON over the argument, because
	// zerolog always writes a "level" key even when WriteLevel is called.
	if ls, ok := fields[zerolog.LevelFieldName].(string); ok {
		lvl, _ := zerolog.ParseLevel(ls)
		r.SetSeverity(zerologLevelToOTel(lvl))
	} else {
		r.SetSeverity(zerologLevelToOTel(level))
	}

	// Message body.
	if msg, ok := fields[zerolog.MessageFieldName].(string); ok {
		r.SetBody(otellog.StringValue(msg))
	}

	// All remaining fields become OTel attributes.
	skip := map[string]bool{
		zerolog.TimestampFieldName: true,
		zerolog.LevelFieldName:     true,
		zerolog.MessageFieldName:   true,
	}
	attrs := make([]otellog.KeyValue, 0, len(fields))
	for k, v := range fields {
		if skip[k] {
			continue
		}
		attrs = append(attrs, otellog.String(k, fmt.Sprint(v)))
	}
	r.AddAttributes(attrs...)

	w.logger.Emit(context.Background(), r)
}

// parseTime tries to parse zerolog's default timestamp format (RFC3339 string
// or Unix float).
func parseTime(v interface{}) (time.Time, bool) {
	switch val := v.(type) {
	case string:
		t, err := time.Parse(time.RFC3339, val)
		if err == nil {
			return t, true
		}
	case float64:
		sec := int64(val)
		nsec := int64((val - float64(sec)) * 1e9)
		return time.Unix(sec, nsec), true
	}
	return time.Time{}, false
}

// zerologLevelToOTel maps zerolog levels to OTel severity values.
func zerologLevelToOTel(level zerolog.Level) otellog.Severity {
	switch level {
	case zerolog.TraceLevel:
		return otellog.SeverityTrace
	case zerolog.DebugLevel:
		return otellog.SeverityDebug
	case zerolog.InfoLevel:
		return otellog.SeverityInfo
	case zerolog.WarnLevel:
		return otellog.SeverityWarn
	case zerolog.ErrorLevel:
		return otellog.SeverityError
	case zerolog.FatalLevel:
		return otellog.SeverityFatal
	case zerolog.PanicLevel:
		return otellog.SeverityFatal4
	default:
		return otellog.SeverityUndefined
	}
}

// Ensure zerologOTelWriter satisfies the zerolog.LevelWriter interface.
var _ io.Writer = (*zerologOTelWriter)(nil)
var _ zerolog.LevelWriter = (*zerologOTelWriter)(nil)
