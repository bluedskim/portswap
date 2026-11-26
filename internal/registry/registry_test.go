package registry

import (
	"context"
	"io"
	"log"
	"os/exec"
	"testing"
	"time"

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

func TestInitialDeployRollsBackOnLaterFailure(t *testing.T) {
	reg := New(log.New(io.Discard, "", 0))
	logger := log.New(io.Discard, "", 0)

	fwdOk, err := forwarder.New("127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go fwdOk.Serve(ctx)

	mgrOk := manager.New(manager.Config{
		Command: []string{"sh", "-c", "trap '' TERM; sleep 120"},
		Logger:  logger,
	}, fwdOk)
	if err := reg.Add("ok", mgrOk, fwdOk); err != nil {
		t.Fatal(err)
	}

	fwdBad, err := forwarder.New("127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	go fwdBad.Serve(ctx)
	mgrBad := manager.New(manager.Config{
		Command: []string{"sh", "-c", "exit 1"},
		Logger:  logger,
	}, fwdBad)
	if err := reg.Add("bad", mgrBad, fwdBad); err != nil {
		t.Fatal(err)
	}

	if err := reg.InitialDeploy(context.Background()); err == nil {
		t.Fatal("expected initial deploy failure")
	}

	st := mgrOk.Status()
	if st.Pid != 0 {
		t.Fatalf("ok service should be shut down, status=%+v", st)
	}
}

func TestInitialDeployRollsBackStopsChildProcess(t *testing.T) {
	reg := New(log.New(io.Discard, "", 0))
	logger := log.New(io.Discard, "", 0)
	fwdOk, _ := forwarder.New("127.0.0.1:0", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go fwdOk.Serve(ctx)
	marker := "portswap-rollback-marker"
	mgrOk := manager.New(manager.Config{
		Command: []string{"sh", "-c", "echo " + marker + " 1>&2; exec sleep 120"},
		Logger:  logger,
	}, fwdOk)
	reg.Add("ok", mgrOk, fwdOk)
	fwdBad, _ := forwarder.New("127.0.0.1:0", nil)
	go fwdBad.Serve(ctx)
	mgrBad := manager.New(manager.Config{
		Command: []string{"false"},
		Logger:  logger,
	}, fwdBad)
	reg.Add("bad", mgrBad, fwdBad)

	if err := reg.InitialDeploy(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
	time.Sleep(200 * time.Millisecond)
	out, _ := exec.Command("pgrep", "-f", marker).CombinedOutput()
	if len(out) > 0 {
		t.Fatalf("child still running: %s", out)
	}
}
