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
	return man.getLogger(name)
}

type loggingService struct {
	mu      sync.Mutex
	level   log.Level
	file    *os.File
	loggers map[string]*log.Logger
}

func (l *loggingService) getLogger(name string) *log.Logger {
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

func (l *loggingService) getLogLevel() log.Level {
	return log.Level(atomic.LoadInt32((*int32)(&l.level)))
}

func (l *loggingService) setLogLevel(level log.Level) {
	atomic.StoreInt32((*int32)(&l.level), int32(level))
}

func (l *loggingService) setLogFile(name string) error {
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

func (l *loggingService) closeLogFile() {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file != nil {
		l.file.Close()
		l.file = nil
	}
}

func (l *loggingService) Write(p []byte) (n int, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	var w io.Writer = l.file

	if l.file == nil {
		w = stdlog.Writer()
	}

	return w.Write(p)
}

func (l *loggingService) Writable(lv log.Level) bool {
	return lv <= l.getLogLevel()
}
