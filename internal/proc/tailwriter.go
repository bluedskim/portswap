package proc

import (
	"io"
	"strings"
	"sync"
)

const (
	defaultTailMaxLines = 20
	defaultTailMaxBytes = 4096
)

// TailWriter 는 dst 로 그대로 쓰면서 최근 출력 일부를 링 버퍼에 남긴다.
type TailWriter struct {
	dst      io.Writer
	maxLines int
	maxBytes int

	mu   sync.Mutex
	raw  []byte
}

// NewTailWriter 는 dst 에 동시에 기록하고 tail 을 유지하는 Writer 를 만든다.
func NewTailWriter(dst io.Writer, maxLines, maxBytes int) *TailWriter {
	if maxLines <= 0 {
		maxLines = defaultTailMaxLines
	}
	if maxBytes <= 0 {
		maxBytes = defaultTailMaxBytes
	}
	return &TailWriter{dst: dst, maxLines: maxLines, maxBytes: maxBytes}
}

// Write 는 dst 에 쓰고 링 버퍼를 갱신한다.
func (t *TailWriter) Write(p []byte) (int, error) {
	n, err := t.dst.Write(p)
	t.mu.Lock()
	t.raw = append(t.raw, p[:n]...)
	if len(t.raw) > t.maxBytes {
		t.raw = append([]byte(nil), t.raw[len(t.raw)-t.maxBytes:]...)
	}
	t.mu.Unlock()
	return n, err
}

// Tail 은 최근 줄(최대 maxLines, 전체 maxBytes 이내)을 돌려준다.
func (t *TailWriter) Tail() string {
	t.mu.Lock()
	s := string(t.raw)
	t.mu.Unlock()
	if s == "" {
		return ""
	}
	s = strings.TrimRight(s, "\n")
	lines := strings.Split(s, "\n")
	if len(lines) > t.maxLines {
		lines = lines[len(lines)-t.maxLines:]
	}
	return strings.Join(lines, "\n")
}