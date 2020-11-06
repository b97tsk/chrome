package service

import (
	"io"
	"log"
	"os"
	"sync"
)

func (man *Manager) Logger(name string) *log.Logger {
	return man.getLogger(name)
}

type loggingService struct {
	mu      sync.Mutex
	file    *os.File
	writer  io.Writer
	loggers map[string]*log.Logger
}

func (l *loggingService) getLogger(name string) *log.Logger {
	logger := l.loggers[name]
	if logger == nil {
		prefix := "[" + name + "] "
		flags := log.Flags() | log.Lmsgprefix
		logger = log.New((*loggingWriter)(l), prefix, flags)
		l.mu.Lock()
		if l.loggers == nil {
			l.loggers = make(map[string]*log.Logger)
		}
		l.loggers[name] = logger
		l.mu.Unlock()
	}
	return logger
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
		file, err := os.OpenFile(name, os.O_APPEND|os.O_CREATE, 0600)
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

type loggingWriter loggingService

func (l *loggingWriter) Write(p []byte) (n int, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.writer
	if w == nil {
		w = log.Writer()
	}
	return w.Write(p)
}
