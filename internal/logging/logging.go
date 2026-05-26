package logging

import (
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/centrifugal/centrifugo/v6/internal/clienttrace"
	"github.com/centrifugal/centrifugo/v6/internal/config"
	"github.com/centrifugal/centrifugo/v6/internal/logutils"

	"github.com/centrifugal/centrifuge"
	"github.com/mattn/go-isatty"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

type Level = zerolog.Level

const (
	TraceLevel = zerolog.TraceLevel
	DebugLevel = zerolog.DebugLevel
	InfoLevel  = zerolog.InfoLevel
	WarnLevel  = zerolog.WarnLevel
	ErrorLevel = zerolog.ErrorLevel
)

var logLevelMatches = map[string]zerolog.Level{
	"NONE":  zerolog.Disabled,
	"TRACE": zerolog.TraceLevel,
	"DEBUG": zerolog.DebugLevel,
	"INFO":  zerolog.InfoLevel,
	"WARN":  zerolog.WarnLevel,
	"ERROR": zerolog.ErrorLevel,
	"FATAL": zerolog.FatalLevel,
}

func CentrifugeLogLevel(level string) centrifuge.LogLevel {
	if l, ok := logStringToLevel[strings.ToLower(level)]; ok {
		return l
	}
	return centrifuge.LogLevelInfo
}

// logStringToLevel matches level string to Centrifuge LogLevel.
var logStringToLevel = map[string]centrifuge.LogLevel{
	"trace": centrifuge.LogLevelTrace,
	"debug": centrifuge.LogLevelDebug,
	"info":  centrifuge.LogLevelInfo,
	"error": centrifuge.LogLevelError,
	"none":  centrifuge.LogLevelNone,
}

type centrifugeLogHandler struct {
	entries chan centrifuge.LogEntry
}

func newCentrifugeLogHandler() *centrifugeLogHandler {
	h := &centrifugeLogHandler{
		entries: make(chan centrifuge.LogEntry, 64),
	}
	go h.readEntries()
	return h
}

func (h *centrifugeLogHandler) readEntries() {
	for entry := range h.entries {
		// Centrifuge LogEntry has no ctx field; for client-scoped log lines
		// the Fields map carries a "client" key with the client ID, which we
		// use to recover the connection's ctx from clienttrace.Get so the
		// emitted zerolog event inherits trace_id / span_id.
		logger := &log.Logger
		if entry.Fields != nil {
			if cid, ok := entry.Fields["client"].(string); ok {
				if ctx := clienttrace.Get(cid); ctx != nil {
					logger = log.Ctx(ctx)
				}
			}
		}

		var l *zerolog.Event
		switch entry.Level {
		case centrifuge.LogLevelTrace:
			l = logger.Trace()
		case centrifuge.LogLevelDebug:
			l = logger.Debug()
		case centrifuge.LogLevelInfo:
			l = logger.Info()
		case centrifuge.LogLevelWarn:
			l = logger.Warn()
		case centrifuge.LogLevelError:
			l = logger.Error()
		default:
			continue
		}
		if entry.Fields != nil {
			if entry.Error != nil {
				l = l.Err(entry.Error)
				delete(entry.Fields, "error")
			}
			l.Fields(entry.Fields).Msg(entry.Message)
		} else {
			if entry.Error != nil {
				l = l.Err(entry.Error)
				delete(entry.Fields, "error")
			}
			l.Msg(entry.Message)
		}
	}
}

func (h *centrifugeLogHandler) Handle(entry centrifuge.LogEntry) {
	select {
	case h.entries <- entry:
	default:
		return
	}
}

func configureConsoleWriter() *zerolog.ConsoleWriter {
	if isTerminalAttached() {
		return &zerolog.ConsoleWriter{
			Out:                 os.Stdout,
			TimeFormat:          "2006-01-02 15:04:05",
			FormatLevel:         logutils.ConsoleFormatLevel(),
			FormatErrFieldName:  logutils.ConsoleFormatErrFieldName(),
			FormatErrFieldValue: logutils.ConsoleFormatErrFieldValue(),
		}
	}
	return nil
}

func isTerminalAttached() bool {
	return isatty.IsTerminal(os.Stdout.Fd()) && runtime.GOOS != "windows"
}

func Setup(cfg config.Config) (centrifuge.LogHandler, func()) {
	var writers []io.Writer

	var file *os.File
	if cfg.Log.File != "" {
		var err error
		file, err = os.OpenFile(cfg.Log.File, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0644) //nolint:gosec // Common for logging.
		if err != nil {
			log.Fatal().Err(err).Msg("error opening log file")
		}
		writers = append(writers, file)
	} else {
		consoleWriter := configureConsoleWriter()
		if consoleWriter != nil {
			writers = append(writers, consoleWriter)
		} else {
			writers = append(writers, os.Stderr)
		}
	}

	levelFound := true
	logLevel, ok := logLevelMatches[strings.ToUpper(cfg.Log.Level)]
	if !ok {
		levelFound = false
		logLevel = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(logLevel)

	if len(writers) > 0 {
		mw := io.MultiWriter(writers...)
		log.Logger = log.Output(mw)
	}

	if !levelFound {
		log.Warn().Msgf("unknown log level %s, defaulting to INFO", cfg.Log.Level)
	}

	return newCentrifugeLogHandler().Handle, func() {
		if file != nil {
			_ = file.Close()
		}
	}
}

// Enabled checks if a specific logging level is enabled
func Enabled(level Level) bool {
	return level >= zerolog.GlobalLevel()
}
