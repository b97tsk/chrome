package service

import (
	"io"
	stdlog "log"
	"os"
	"sync"
	"sync/atomic"

	"github.com/b97tsk/chrome/internal/log"
)

func (man *Manager) Logger(name string) *log.Logger {
	return man.builtin.Logger(name)
}

func (man *Manager) SetLogOutput(w io.Writer) {
	man.builtin.SetLogOutput(w)
}

type loggingService struct {
	mu      sync.Mutex
	level   log.Level
	file    *os.File
	output  io.Writer
	loggers map[string]*log.Logger
}

func (l *loggingService) Logger(name string) *log.Logger {
	l.mu.Lock()
	defer l.mu.Unlock()

	logger := l.loggers[name]
	if logger == nil {
		logger = log.New(l, "["+name+"] ", stdlog.Flags()|stdlog.Lmsgprefix)

		if l.loggers == nil {
			l.loggers = make(map[string]*log.Logger)
		}

		l.loggers[name] = logger
	}

	return logger
}

func (l *loggingService) LogLevel() log.Level {
	return log.Level(atomic.LoadInt32((*int32)(&l.level)))
}

func (l *loggingService) SetLogLevel(level log.Level) {
	atomic.StoreInt32((*int32)(&l.level), int32(level))
}

func (l *loggingService) SetLogFile(name string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file != nil {
		l.file.Close()
		l.file = nil
	}

	if name != "" {
		file, err := os.OpenFile(name, os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			return err
		}

		l.file = file
	}

	return nil
}

func (l *loggingService) SetLogOutput(w io.Writer) {
	l.mu.Lock()
	l.output = w
	l.mu.Unlock()
}

func (l *loggingService) Write(p []byte) (n int, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file != nil {
		if _, err := l.file.Write(p); err != nil {
			l.file.Close()
			l.file = nil
		}
	}

	if l.output != nil {
		if _, err := l.output.Write(p); err != nil {
			l.output = nil
		}
	}

	return len(p), nil
}

func (l *loggingService) Writable(lv log.Level) bool {
	return lv <= l.LogLevel()
}
