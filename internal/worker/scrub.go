package worker

import (
	"io"
	"strings"
	"sync"

	"attesttag/internal/app"
)

// Scrubber masks the secrets this process holds (exact values) and anything secret-shaped
// (the bot's patterns) in every string that leaves it: events, the result, the PR body, logs.
type Scrubber struct {
	mu     sync.RWMutex
	values []string
}

func newScrubber(values ...string) *Scrubber {
	s := &Scrubber{}
	s.add(values...)
	return s
}

func (s *Scrubber) add(values ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range values {
		if len(v) >= 6 {
			s.values = append(s.values, v)
		}
	}
}

func (s *Scrubber) Clean(text string) string {
	s.mu.RLock()
	for _, v := range s.values {
		text = strings.ReplaceAll(text, v, "[redacted-secret]")
	}
	s.mu.RUnlock()
	return app.Redact(text)
}

// scrubWriter cleans log lines on their way out.
type scrubWriter struct {
	w io.Writer
	s *Scrubber
}

func (w *scrubWriter) Write(p []byte) (int, error) {
	if _, err := w.w.Write([]byte(w.s.Clean(string(p)))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// tailBuffer keeps the first head bytes and the last tail bytes of a stream, so a log that runs
// to megabytes still yields its start (the command) and its end (the failure).
type tailBuffer struct {
	mu   sync.Mutex
	head []byte
	tail []byte
	max  int // head cap
	keep int // tail cap
	n    int
}

func newTailBuffer(head, tail int) *tailBuffer { return &tailBuffer{max: head, keep: tail} }

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.n += len(p)
	if len(b.head) < b.max {
		take := b.max - len(b.head)
		if take > len(p) {
			take = len(p)
		}
		b.head = append(b.head, p[:take]...)
	}
	b.tail = append(b.tail, p...)
	if len(b.tail) > b.keep {
		b.tail = append([]byte{}, b.tail[len(b.tail)-b.keep:]...)
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.n <= b.max {
		return string(b.head)
	}
	if b.n <= b.max+b.keep {
		return string(b.head) + string(b.tail[len(b.tail)-(b.n-len(b.head)):])
	}
	return string(b.head) + "\n…[" + itoa(b.n-len(b.head)-len(b.tail)) + " bytes omitted]…\n" + string(b.tail)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	if neg {
		d = append([]byte{'-'}, d...)
	}
	return string(d)
}

// cut trims s to at most n bytes, keeping the start, and says so.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…[truncated]"
}

// lastLines keeps the final n bytes of s, starting at a line boundary when possible.
func lastLines(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	if i := strings.IndexByte(s, '\n'); i >= 0 && i < len(s)-1 {
		s = s[i+1:]
	}
	return "…\n" + s
}
