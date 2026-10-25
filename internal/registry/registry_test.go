package registry

import (
	"context"
	"io"
	"log"
	"testing"

	"portswap/internal/forwarder"
	"portswap/internal/manager"
)

func TestNamesAndCountDoNotPanic(t *testing.T) {
	reg := New(log.New(io.Discard, "", 0))
	fwd, err := forwarder.New("127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	mgr := manager.New(manager.Config{
		Command: []string{"python3", "-c", "import time; time.sleep(1)"},
		Logger:  log.New(io.Discard, "", 0),
	}, fwd)
	if err := reg.Add("a", mgr, fwd); err != nil {
		t.Fatal(err)
	}
	if reg.Count() != 1 || len(reg.Names()) != 1 {
		t.Fatalf("count=%d names=%v", reg.Count(), reg.Names())
	}
	reg.Shutdown(context.Background())
}
