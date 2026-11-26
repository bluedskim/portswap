package registry

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"portswap/internal/forwarder"
	"portswap/internal/manager"
	"portswap/internal/proc"
)

func TestInitialDeployFailureShutsDownPriorServices(t *testing.T) {
	reg := New(log.New(io.Discard, "", 0))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	add := func(name string, cmd []string, healthTimeout time.Duration) error {
		fwd, err := forwarder.New("127.0.0.1:0", nil)
		if err != nil {
			return err
		}
		mgr := manager.New(manager.Config{
			Command:        cmd,
			Health:         proc.TCPHealth{},
			HealthTimeout:  healthTimeout,
			HealthInterval: 50 * time.Millisecond,
			Grace:          2 * time.Second,
			Logger:         log.New(io.Discard, "", 0),
		}, fwd)
		return reg.Add(name, mgr, fwd)
	}

	listenPy := `import socket,os,time;s=socket.socket();s.bind(("127.0.0.1",int(os.environ["PORT"])));s.listen();time.sleep(300)`
	if err := add("good", []string{"python3", "-c", listenPy}, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := add("bad", []string{"sh", "-c", "exit 1"}, 500*time.Millisecond); err != nil {
		t.Fatal(err)
	}

	reg.StartForwarders(ctx)
	if err := reg.InitialDeploy(ctx); err == nil {
		t.Fatal("expected initial deploy failure")
	}

	good, err := reg.EntryFor("good")
	if err != nil {
		t.Fatal(err)
	}
	if cur := good.Mgr.Current(); cur != nil && !cur.Exited() {
		t.Fatalf("good service pid %d should be stopped", cur.Pid())
	}
}