package ioutil

import "io"

func LimitWriter(w io.Writer, n int64, rest io.Writer) io.Writer {
	return &limitedWriter{w, n, rest}
}

type limitedWriter struct {
	w    io.Writer
	n    int64
	rest io.Writer
}

func (l *limitedWriter) Write(p []byte) (n int, err error) {
	if l.n <= 0 {
		if l.rest != nil {
			return l.rest.Write(p)
		}

		return len(p), nil
	}

	var rest []byte

	if l.n < int64(len(p)) {
		p, rest = p[:l.n], p[l.n:]
	}

	n, err = l.w.Write(p)
	l.n -= int64(n)

	if err != nil {
		return
	}

	if len(rest) > 0 && l.rest != nil {
		if n2, err2 := l.rest.Write(rest); err2 != nil {
			return n + n2, err2
		}
	}

	return len(p), nil
}
