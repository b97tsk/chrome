package log

import (
	"bytes"
	"io"
	"log"
	"sync"
)

type Writer interface {
	io.Writer
	Writable(Level) bool
}

type Logger struct {
	Writer

	e *log.Logger
	w *log.Logger
	i *log.Logger
	d *log.Logger
	t *log.Logger
}

const levelPlaceHolder = "[#####]"

func New(w Writer, prefix string, flags int) *Logger {
	prefix = levelPlaceHolder + " " + prefix

	return &Logger{
		Writer: w,

		e: log.New(&writer{w, LevelError}, prefix, flags),
		w: log.New(&writer{w, LevelWarn}, prefix, flags),
		i: log.New(&writer{w, LevelInfo}, prefix, flags),
		d: log.New(&writer{w, LevelDebug}, prefix, flags),
		t: log.New(&writer{w, LevelTrace}, prefix, flags),
	}
}

func (l *Logger) Error(v ...interface{}) {
	if l.ErrorWritable() {
		l.e.Print(v...)
	}
}

func (l *Logger) Errorf(format string, v ...interface{}) {
	if l.ErrorWritable() {
		l.e.Printf(format, v...)
	}
}

func (l *Logger) ErrorWritable() bool {
	return l.Writable(LevelError)
}

func (l *Logger) Warn(v ...interface{}) {
	if l.WarnWritable() {
		l.w.Print(v...)
	}
}

func (l *Logger) Warnf(format string, v ...interface{}) {
	if l.WarnWritable() {
		l.w.Printf(format, v...)
	}
}

func (l *Logger) WarnWritable() bool {
	return l.Writable(LevelWarn)
}

func (l *Logger) Info(v ...interface{}) {
	if l.InfoWritable() {
		l.i.Print(v...)
	}
}

func (l *Logger) Infof(format string, v ...interface{}) {
	if l.InfoWritable() {
		l.i.Printf(format, v...)
	}
}

func (l *Logger) InfoWritable() bool {
	return l.Writable(LevelInfo)
}

func (l *Logger) Debug(v ...interface{}) {
	if l.DebugWritable() {
		l.d.Print(v...)
	}
}

func (l *Logger) Debugf(format string, v ...interface{}) {
	if l.DebugWritable() {
		l.d.Printf(format, v...)
	}
}

func (l *Logger) DebugWritable() bool {
	return l.Writable(LevelDebug)
}

func (l *Logger) Trace(v ...interface{}) {
	if l.TraceWritable() {
		l.t.Print(v...)
	}
}

func (l *Logger) Tracef(format string, v ...interface{}) {
	if l.TraceWritable() {
		l.t.Printf(format, v...)
	}
}

func (l *Logger) TraceWritable() bool {
	return l.Writable(LevelTrace)
}

type writer struct {
	w  Writer
	lv Level
}

func (w *writer) Write(p []byte) (n int, err error) {
	if i := bytes.Index(p, []byte(levelPlaceHolder)); i >= 0 {
		b := pool.Get().(*buffer)
		defer pool.Put(b)

		s := (*b)[:0]
		s = append(s, p[:i]...)
		s = append(s, '[')
		s = append(s, w.lv.String()...)
		s = append(s, ']')
		s = append(s, p[i+len(levelPlaceHolder):]...)
		p = s
	}

	return w.w.Write(p)
}

type buffer [1024]byte

var pool = sync.Pool{
	New: func() interface{} { return new(buffer) },
}
