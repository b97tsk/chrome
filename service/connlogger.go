package service

import (
	"fmt"
	"log"
	"net"
)

type ConnLogger struct {
	net.Conn

	prefix string
	total  struct {
		reads, writes int64
	}
}

func NewConnLogger(c net.Conn, prefix string) *ConnLogger {
	l := &ConnLogger{
		Conn:   c,
		prefix: prefix,
	}
	l.Log("new connection")
	return l
}

func (l *ConnLogger) Log(a ...interface{}) {
	log.Print(l.prefix, fmt.Sprint(a...))
}

func (l *ConnLogger) Logf(format string, a ...interface{}) {
	log.Print(l.prefix, fmt.Sprintf(format, a...))
}

func (l *ConnLogger) Read(p []byte) (n int, err error) {
	n, err = l.Conn.Read(p)
	if n > 0 {
		l.total.reads += int64(n)
		l.Logf("read: +%v bytes (%v)", n, l.total.reads)
	}
	return
}

func (l *ConnLogger) Write(p []byte) (n int, err error) {
	n, err = l.Conn.Write(p)
	if n > 0 {
		l.total.writes += int64(n)
		l.Logf("write: +%v bytes (%v)", n, l.total.writes)
	}
	return
}

func (l *ConnLogger) Close() error {
	err := l.Conn.Close()
	l.Logf("close: %v / %v bytes", l.total.reads, l.total.writes)
	return err
}
