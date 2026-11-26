package proc

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestStderrTailStreamsAndSnapshots(t *testing.T) {
	var dest bytes.Buffer
	tail, w := NewStderrTail(&dest, 256, 3)
	for i := 1; i <= 5; i++ {
		fmt.Fprintf(w, "line%d\n", i)
	}
	if got := dest.String(); !strings.Contains(got, "line5") {
		t.Fatalf("dest=%q", got)
	}
	snap := tail.Snapshot()
	if !strings.Contains(snap, "line3") || !strings.Contains(snap, "line5") || strings.Contains(snap, "line1") {
		t.Fatalf("snap=%q", snap)
	}
}

func TestAppendHealthStderr(t *testing.T) {
	tail, w := NewStderrTail(io.Discard, 1024, 20)
	fmt.Fprint(w, "boot failed: no config\n")
	base := errors.New("프로세스가 헬스체크 전에 종료됨")
	wrapped := AppendHealthStderr(base, tail)
	if !strings.Contains(wrapped.Error(), "stderr") || !strings.Contains(wrapped.Error(), "boot failed") {
		t.Fatalf("err=%q", wrapped.Error())
	}
	if !errors.Is(wrapped, base) {
		t.Fatal("expected unwrap")
	}
}