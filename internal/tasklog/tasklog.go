// Package tasklog writes timestamped task logs to files with secret redaction.
package tasklog

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Logger struct {
	mu      sync.Mutex
	f       *os.File
	path    string
	secrets []string
	tail    []string
}

func Open(path string) (*Logger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, err
	}
	return &Logger{f: f, path: path}, nil
}

// Discard returns a logger that writes nowhere (used in tests / dry operations).
func Discard() *Logger { return &Logger{} }

func (l *Logger) Path() string { return l.path }

// Redact registers secrets that must never appear in the log.
func (l *Logger) Redact(secrets ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range secrets {
		if len(s) >= 3 {
			l.secrets = append(l.secrets, s)
		}
	}
}

func (l *Logger) line(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, sec := range l.secrets {
		s = strings.ReplaceAll(s, sec, "******")
	}
	ts := time.Now().Format("2006-01-02 15:04:05")
	if l.f != nil {
		fmt.Fprintf(l.f, "%s %s\n", ts, s)
	}
	l.tail = append(l.tail, s)
	if len(l.tail) > 60 {
		l.tail = l.tail[len(l.tail)-60:]
	}
}

func (l *Logger) Printf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	for _, ln := range strings.Split(strings.TrimRight(msg, "\n"), "\n") {
		l.line(ln)
	}
}

// Tail returns the last n log lines (used to build error summaries).
func (l *Logger) Tail(n int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n > len(l.tail) {
		n = len(l.tail)
	}
	out := make([]string, n)
	copy(out, l.tail[len(l.tail)-n:])
	return out
}

func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		err := l.f.Close()
		l.f = nil
		return err
	}
	return nil
}

// Writer returns an io.WriteCloser splitting output into prefixed log lines.
// Carriage-return progress output is collapsed so logs stay readable.
func (l *Logger) Writer(prefix string) io.WriteCloser {
	return &lineWriter{l: l, prefix: prefix}
}

type lineWriter struct {
	l      *Logger
	prefix string
	buf    bytes.Buffer
	mu     sync.Mutex
	lines  int
}

const maxLinesPerWriter = 20000

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	for {
		b := w.buf.Bytes()
		i := bytes.IndexAny(b, "\n\r")
		if i < 0 {
			if w.buf.Len() > 64*1024 {
				w.emit(string(b))
				w.buf.Reset()
			}
			break
		}
		ln := string(b[:i])
		w.buf.Next(i + 1)
		w.emit(ln)
	}
	return len(p), nil
}

func (w *lineWriter) emit(ln string) {
	ln = strings.TrimRight(ln, " \t")
	if ln == "" {
		return
	}
	w.lines++
	if w.lines == maxLinesPerWriter {
		w.l.line(w.prefix + "... (output truncated)")
	}
	if w.lines >= maxLinesPerWriter {
		return
	}
	w.l.line(w.prefix + ln)
}

func (w *lineWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf.Len() > 0 {
		w.emit(w.buf.String())
		w.buf.Reset()
	}
	return nil
}

// ReadChunk reads a log file from offset, returning at most max bytes.
func ReadChunk(path string, offset int64, max int) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", 0, nil
		}
		return "", 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	size := st.Size()
	if offset < 0 {
		// negative offset = tail
		offset = size + offset
		if offset < 0 {
			offset = 0
		}
	}
	if offset > size {
		offset = size
	}
	if max <= 0 {
		max = 256 * 1024
	}
	n := size - offset
	if n > int64(max) {
		n = int64(max)
	}
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, offset); err != nil && err != io.EOF {
		return "", offset, err
	}
	return string(buf), offset + n, nil
}
