package chrome

import (
	"io"
	stdlog "log"
	"os"
	"sync"
	"sync/atomic"

	"github.com/b97tsk/log"
)

type loggingService struct {
	mu      sync.Mutex
	level   atomic.Int32
	file    *os.File
	output  io.Writer
	loggers sync.Map
}

// Logger gets a Logger with name quoted with square brackets as the prefix.
func (m *loggingService) Logger(name string) *log.Logger {
	logger, ok := m.loggers.Load(name)
	if !ok {
		logger = log.New(loggingWriter{m}, "["+name+"] ", stdlog.LstdFlags|stdlog.Lmsgprefix)
		logger, _ = m.loggers.LoadOrStore(name, logger)
	}

	return logger.(*log.Logger)
}

// LogLevel gets the logging level.
func (m *loggingService) LogLevel() log.Level {
	return log.Level(m.level.Load())
}

// SetLogLevel sets the logging level.
func (m *loggingService) SetLogLevel(level log.Level) {
	m.level.Store(int32(level))
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

// SetLogOutput sets a io.Writer that each logging message would write to.
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

func (m loggingWriter) Writable(lv log.Level) bool {
	return lv >= m.LogLevel()
}
