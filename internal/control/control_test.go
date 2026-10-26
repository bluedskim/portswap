package control

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"portswap/internal/forwarder"
	"portswap/internal/manager"
)

func TestStatusAndDeployOverUnixSocket(t *testing.T) {
	fwd, err := forwarder.New("127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go fwd.Serve(ctx)

	// nc 가 없을 수 있어 python3 로 TCP 리슨만 하는 앱을 쓴다.
	mgr := manager.New(manager.Config{
		Command:        []string{"python3", "-c", "import socket,time,os;s=socket.socket();s.bind(('127.0.0.1',int(os.environ['PORT'])));s.listen();time.sleep(60)"},
		HealthTimeout:  5 * time.Second,
		HealthInterval: 50 * time.Millisecond,
		Grace:          time.Second,
		Logger:         log.New(io.Discard, "", 0),
	}, fwd)
	defer mgr.Shutdown(context.Background())

	sock := filepath.Join(t.TempDir(), "ctl.sock")
	srv := NewServer(mgr, sock)
	go srv.ListenAndServe(ctx)
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	cl := NewClient(sock)
	st, err := cl.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Pid != 0 || st.Deploys != 0 {
		t.Fatalf("unexpected initial status %+v", st)
	}

	res, err := cl.Deploy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Status.Pid == 0 || res.Status.Deploys != 1 {
		t.Fatalf("deploy result %+v", res)
	}
	first := res.Status.Pid

	res, err = cl.Deploy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status.Pid == first || res.Status.Deploys != 2 {
		t.Fatalf("second deploy %+v", res)
	}

	cancel()
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(sock); err == nil {
		t.Fatal("socket should be removed on shutdown")
	}
}

func TestDeployFailureReported(t *testing.T) {
	fwd, _ := forwarder.New("127.0.0.1:0", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go fwd.Serve(ctx)
	mgr := manager.New(manager.Config{
		Command:        []string{"sh", "-c", "exit 1"},
		HealthTimeout:  time.Second,
		HealthInterval: 50 * time.Millisecond,
		Logger:         log.New(io.Discard, "", 0),
	}, fwd)
	sock := filepath.Join(t.TempDir(), "ctl.sock")
	go NewServer(mgr, sock).ListenAndServe(ctx)
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	res, err := NewClient(sock).Deploy(ctx)
	if err == nil || res.OK {
		t.Fatalf("expected failure, got %+v err=%v", res, err)
	}
}
