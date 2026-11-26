package proc

import (
	"bytes"
	"io"
	"strings"
	"sync"
)

const (
	defaultStderrTailBytes = 4096
	defaultStderrTailLines   = 20
)

// StderrTail 은 stderr 의 마지막 구간만 보관한다. 실제 출력은 MultiWriter 의 다른 대상이 받는다.
type StderrTail struct {
	maxBytes int
	maxLines int

	mu  sync.Mutex
	buf bytes.Buffer
}

// NewStderrTail 은 dest 로 그대로 쓰면서 tail 에 마지막 출력을 쌓는 Writer 를 만든다.
func NewStderrTail(dest io.Writer, maxBytes, maxLines int) (*StderrTail, io.Writer) {
	if maxBytes <= 0 {
		maxBytes = defaultStderrTailBytes
	}
	if maxLines <= 0 {
		maxLines = defaultStderrTailLines
	}
	if dest == nil {
		dest = io.Discard
	}
	t := &StderrTail{maxBytes: maxBytes, maxLines: maxLines}
	return t, io.MultiWriter(dest, t)
}

func (t *StderrTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.buf.Write(p)
	if t.buf.Len() > t.maxBytes {
		excess := t.buf.Len() - t.maxBytes
		remain := t.buf.Bytes()[excess:]
		t.buf.Reset()
		t.buf.Write(remain)
	}
	t.mu.Unlock()
	return len(p), nil
}

// Snapshot 은 보관 중인 stderr 의 마지막 maxLines 줄(전체는 maxBytes 이내)을 돌려준다.
func (t *StderrTail) Snapshot() string {
	t.mu.Lock()
	raw := t.buf.String()
	t.mu.Unlock()
	if raw == "" {
		return ""
	}
	lines := strings.Split(raw, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > t.maxLines {
		lines = lines[len(lines)-t.maxLines:]
	}
	return strings.Join(lines, "\n")
}

// AppendHealthStderr 는 헬스체크 실패 오류에 stderr 꼬리를 붙인다. tail 이 비어 있으면 err 그대로다.
func AppendHealthStderr(err error, tail *StderrTail) error {
	if err == nil || tail == nil {
		return err
	}
	s := tail.Snapshot()
	if s == "" {
		return err
	}
	return &healthErr{err: err, stderr: s}
}

type healthErr struct {
	err    error
	stderr string
}

func (e *healthErr) Error() string {
	return e.err.Error() + "\n--- stderr (마지막 출력) ---\n" + e.stderr
}

func (e *healthErr) Unwrap() error { return e.err }