package proc

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
)

func TestTailWriterKeepsLastLines(t *testing.T) {
	var dst bytes.Buffer
	tw := NewTailWriter(&dst, 3, 4096)
	for i := 1; i <= 5; i++ {
		tw.Write([]byte("line" + strconv.Itoa(i) + "\n"))
	}
	if got := tw.Tail(); got != "line3\nline4\nline5" {
		t.Fatalf("tail=%q dst=%q", got, dst.String())
	}
	wantDst := strings.Join([]string{"line1", "line2", "line3", "line4", "line5"}, "\n") + "\n"
	if dst.String() != wantDst {
		t.Fatalf("dst lost streaming: %q", dst.String())
	}
}

func TestTailWriterMaxBytes(t *testing.T) {
	var dst bytes.Buffer
	tw := NewTailWriter(&dst, 100, 32)
	payload := strings.Repeat("x", 80)
	tw.Write([]byte(payload))
	tw.Write([]byte(payload))
	if len(tw.Tail()) > 32 {
		t.Fatalf("tail too long: %d", len(tw.Tail()))
	}
}