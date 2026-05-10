package chrome

import (
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

type loggingService struct {
	mu     sync.Mutex
	level  slog.LevelVar
	logger atomic.Pointer[slog.Logger]
	file   *os.File
	output io.Writer
}

// Logger returns the Logger of m.
func (m *loggingService) Logger() *slog.Logger {
	logger := m.logger.Load()
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(loggingWriter{m}, &slog.HandlerOptions{
			Level: &m.level,
			ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
				if a.Key == slog.TimeKey {
					return slog.String(slog.TimeKey, a.Value.Time().Format(time.RFC3339))
				}
				return a
			},
		}))
		if !m.logger.CompareAndSwap(nil, logger) {
			logger = m.logger.Load()
		}
	}
	return logger
}

// LogLevel gets the logging level.
func (m *loggingService) LogLevel() slog.Level {
	return m.level.Level()
}

// SetLogLevel sets the logging level.
func (m *loggingService) SetLogLevel(level slog.Level) {
	m.level.Set(level)
}

// SetLogFile sets the file that each logging message would write to.
func (m *loggingService) SetLogFile(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.file != nil {
		m.file.Close()
		m.file = nil
	}

	if name != "" {
		file, err := os.OpenFile(name, os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			return err
		}

		m.file = file
	}

	return nil
}

// SetLogOutput sets a [io.Writer] that each logging message would write to.
func (m *loggingService) SetLogOutput(w io.Writer) {
	m.mu.Lock()
	m.output = w
	m.mu.Unlock()
}

type loggingWriter struct {
	*loggingService
}

func (m loggingWriter) Write(p []byte) (n int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.file != nil {
		if _, err := m.file.Write(p); err != nil {
			m.file.Close()
			m.file = nil
		}
	}

	if m.output != nil {
		if _, err := m.output.Write(p); err != nil {
			m.output = nil
		}
	}

	return len(p), nil
}
