// Package daemonlog merges a supervised daemon's stdout and stderr into one
// ordered, timestamped local log, and redacts common secret-bearing patterns
// before they reach disk (#4368).
//
// Splitting a process's stdout and stderr into two separate files loses
// their relative ordering — the chronological startup story a diagnosis
// needs is scattered across two files with no way to interleave them back
// together after the fact. MergedWriter fixes this by giving both streams
// the same underlying io.Writer: since Go's exec package serializes writes
// through whatever io.Writer is handed to it, using one Writer instance for
// both cmd.Stdout and cmd.Stderr preserves the actual interleaving of
// messages as they happen, not just as they're later concatenated.
package daemonlog

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"sync"
	"time"
)

// MergedWriter timestamps and line-buffers everything written to it (from
// however many concurrent writers — a daemon's stdout and its stderr, for
// example) before forwarding each complete line, in the order it completed,
// to the underlying writer. Safe for concurrent use.
type MergedWriter struct {
	mu      sync.Mutex
	out     io.Writer
	pending []byte
	now     func() time.Time
}

// NewMergedWriter returns a MergedWriter forwarding to out.
func NewMergedWriter(out io.Writer) *MergedWriter {
	return &MergedWriter{out: out, now: time.Now}
}

// Write implements io.Writer. It never returns a short count for a nil
// error: any complete lines in p are flushed with a timestamp prefix, and
// any trailing partial line is buffered until a later Write or Close
// completes it.
func (m *MergedWriter) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending = append(m.pending, p...)
	for {
		i := bytes.IndexByte(m.pending, '\n')
		if i < 0 {
			break
		}
		line := m.pending[:i]
		m.pending = m.pending[i+1:]
		if _, err := m.writeLineLocked(line); err != nil {
			return len(p), err
		}
	}
	return len(p), nil
}

// Close flushes any buffered partial line (one with no trailing newline at
// process exit) and closes the underlying writer if it implements io.Closer.
func (m *MergedWriter) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.pending) > 0 {
		if _, err := m.writeLineLocked(m.pending); err != nil {
			return err
		}
		m.pending = nil
	}
	if closer, ok := m.out.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func (m *MergedWriter) writeLineLocked(line []byte) (int, error) {
	return fmt.Fprintf(m.out, "%s %s\n", m.now().UTC().Format(time.RFC3339Nano), Redact(string(line)))
}

var (
	// basicAuthURLPattern matches userinfo credentials embedded in a URL
	// (scheme://user:pass@host), the shape a git remote or webhook error
	// message can otherwise leak verbatim into a log.
	basicAuthURLPattern = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/@\s]+@`)
	// authHeaderPattern matches an Authorization header value, case-insensitive.
	authHeaderPattern = regexp.MustCompile(`(?i)(authorization:\s*)\S+`)
	// bearerTokenPattern matches a bare "Bearer <token>" appearing outside a
	// header line (e.g. quoted in an error message).
	bearerTokenPattern = regexp.MustCompile(`(?i)(bearer\s+)\S+`)
	// tokenLikeQueryPattern matches token/secret/key/password-shaped query
	// parameters or key=value pairs (env values included).
	tokenLikeQueryPattern = regexp.MustCompile(`(?i)((?:access[_-]?token|api[_-]?key|secret|password|token)\s*[:=]\s*)\S+`)
)

// Redact strips credentials, Authorization headers, token-bearing URLs, and
// token/secret/password-shaped key-value pairs from line before it is
// logged (#4368's "keep credentials ... out of all logs").
func Redact(line string) string {
	line = basicAuthURLPattern.ReplaceAllString(line, "${1}[redacted]@")
	// Bearer tokens before the header pattern: "Authorization: Bearer <tok>"
	// must have <tok> gone before authHeaderPattern's single \S+ swallows
	// only "Bearer" and leaves the token behind as trailing text.
	line = bearerTokenPattern.ReplaceAllString(line, "${1}[redacted]")
	line = authHeaderPattern.ReplaceAllString(line, "${1}[redacted]")
	line = tokenLikeQueryPattern.ReplaceAllString(line, "${1}[redacted]")
	return line
}
