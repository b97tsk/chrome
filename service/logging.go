package service

import (
	"bytes"
	"io"
	"log"
	"os"
	"sync"
	"sync/atomic"
)

type Logger struct {
	ERROR *log.Logger
	WARN  *log.Logger
	INFO  *log.Logger
	DEBUG *log.Logger
	TRACE *log.Logger
}

func (man *Manager) Logger(name string) Logger {
	return man.getLogger(name)
}

const logLevelPlaceHolder = "[#####]"

type loggingService struct {
	mu      sync.Mutex
	level   logLevel
	file    *os.File
	writer  io.Writer
	loggers map[string]Logger
}

func (l *loggingService) getLogger(name string) Logger {
	l.mu.Lock()
	defer l.mu.Unlock()

	logger, ok := l.loggers[name]
	if !ok {
		prefix := logLevelPlaceHolder + " [" + name + "] "
		flags := log.Flags() | log.Lmsgprefix

		logger.ERROR = log.New(&loggingWriter{l, logLevelError}, prefix, flags)
		logger.WARN = log.New(&loggingWriter{l, logLevelWarn}, prefix, flags)
		logger.INFO = log.New(&loggingWriter{l, logLevelInfo}, prefix, flags)
		logger.DEBUG = log.New(&loggingWriter{l, logLevelDebug}, prefix, flags)
		logger.TRACE = log.New(&loggingWriter{l, logLevelTrace}, prefix, flags)

		if l.loggers == nil {
			l.loggers = make(map[string]Logger)
		}

		l.loggers[name] = logger
	}

	return logger
}

func (l *loggingService) getLogLevel() logLevel {
	return logLevel(atomic.LoadInt32((*int32)(&l.level)))
}

func (l *loggingService) setLogLevel(level logLevel) {
	atomic.StoreInt32((*int32)(&l.level), int32(level))
}

func (l *loggingService) setLogFile(name string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file != nil {
		l.file.Close()
		l.file = nil
		l.writer = nil
	}

	if name != "" {
		file, err := os.OpenFile(name, os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			return err
		}

		l.file = file
		l.writer = io.MultiWriter(file, log.Writer())
	}

	return nil
}

func (l *loggingService) closeLogFile() {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file != nil {
		l.file.Close()
		l.file = nil
		l.writer = nil
	}
}

type loggingWriter struct {
	*loggingService
	level logLevel
}

func (l loggingWriter) Write(p []byte) (n int, err error) {
	if l.level > l.getLogLevel() {
		return len(p), nil
	}

	if i := bytes.Index(p, []byte(logLevelPlaceHolder)); i >= 0 {
		b := loggingPool.Get().(*loggingBuffer)
		defer loggingPool.Put(b)

		s := (*b)[:0]
		s = append(s, p[:i]...)
		s = append(s, []byte("["+l.level.String()+"]")...)
		s = append(s, p[i+len(logLevelPlaceHolder):]...)
		p = s
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	w := l.writer
	if w == nil {
		w = log.Writer()
	}

	return w.Write(p)
}

type loggingBuffer [1024]byte

var loggingPool = sync.Pool{
	New: func() interface{} { return new(loggingBuffer) },
}
