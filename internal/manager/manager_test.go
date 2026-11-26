package manager

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"portswap/internal/forwarder"
	"portswap/internal/proc"
)

var demoBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "portswap-test")
	if err != nil {
		panic(err)
	}
	demoBin = filepath.Join(dir, "demoapp")
	build := exec.Command("go", "build", "-o", demoBin, "../../examples/demoapp")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func newManager(t *testing.T, cfg Config) (*Manager, *forwarder.Forwarder) {
	t.Helper()
	fwd, err := forwarder.New("127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go fwd.Serve(ctx)
	if cfg.Logger == nil {
		cfg.Logger = log.New(io.Discard, "", 0)
	}
	if cfg.Health == nil {
		cfg.Health = proc.HTTPHealth{URLTemplate: "http://127.0.0.1:{port}/health"}
	}
	if cfg.HealthTimeout == 0 {
		cfg.HealthTimeout = 5 * time.Second
	}
	cfg.HealthInterval = 50 * time.Millisecond
	if cfg.Grace == 0 {
		cfg.Grace = 3 * time.Second
	}
	m := New(cfg, fwd)
	t.Cleanup(func() {
		m.Shutdown(context.Background())
		cancel()
	})
	return m, fwd
}

func get(t *testing.T, url string) string {
	t.Helper()
	cl := http.Client{Timeout: 5 * time.Second}
	resp, err := cl.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestDeploySwitchesAndKillsOld(t *testing.T) {
	m, fwd := newManager(t, Config{Command: []string{demoBin, "-version", "v1"}})
	p1, err := m.Deploy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + fwd.Addr().String()
	if got := get(t, base+"/"); !strings.Contains(got, fmt.Sprintf("pid=%d version=v1", p1.Pid())) {
		t.Fatalf("got %q", got)
	}

	m.cfg.Command = []string{demoBin, "-version", "v2"}
	p2, err := m.Deploy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := get(t, base+"/"); !strings.Contains(got, fmt.Sprintf("pid=%d version=v2", p2.Pid())) {
		t.Fatalf("got %q", got)
	}
	if !p1.Exited() {
		t.Fatal("old process should be terminated")
	}
	st := m.Status()
	if st.Deploys != 2 || st.Pid != p2.Pid() || st.Backend != p2.Addr {
		t.Fatalf("status %+v", st)
	}
}

func TestFailedHealthIncludesAppOutputTail(t *testing.T) {
	m, _ := newManager(t, Config{Command: []string{demoBin, "-version", "v1"}})
	if _, err := m.Deploy(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.cfg.Command = []string{demoBin, "-version", "tailtest", "-fail-health"}
	_, err := m.Deploy(context.Background())
	if err == nil {
		t.Fatal("expected health failure")
	}
	if !strings.Contains(err.Error(), "demoapp") {
		t.Fatalf("error should include app stderr tail: %q", err)
	}
}

func TestFailedHealthKeepsOldProcess(t *testing.T) {
	m, fwd := newManager(t, Config{Command: []string{demoBin, "-version", "v1"}, HealthTimeout: 700 * time.Millisecond})
	p1, err := m.Deploy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m.cfg.Command = []string{demoBin, "-version", "v2", "-fail-health"}
	if _, err := m.Deploy(context.Background()); err == nil {
		t.Fatal("expected health failure")
	}
	if p1.Exited() {
		t.Fatal("old process must survive a failed deploy")
	}
	if fwd.Backend() != p1.Addr {
		t.Fatalf("backend changed to %s", fwd.Backend())
	}
	got := get(t, "http://"+fwd.Addr().String()+"/")
	if !strings.Contains(got, "version=v1") {
		t.Fatalf("got %q", got)
	}
}

func TestInFlightRequestCompletesOnOldProcessDuringDeploy(t *testing.T) {
	m, fwd := newManager(t, Config{Command: []string{demoBin, "-version", "v1"}, Drain: 10 * time.Second})
	p1, err := m.Deploy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + fwd.Addr().String()

	var wg sync.WaitGroup
	var slowResp string
	wg.Add(1)
	go func() {
		defer wg.Done()
		slowResp = get(t, base+"/slow") // 3초 걸리는 요청
	}()
	time.Sleep(300 * time.Millisecond) // 요청이 옛 프로세스에 도달하도록

	m.cfg.Command = []string{demoBin, "-version", "v2"}
	start := time.Now()
	p2, err := m.Deploy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Deploy 는 drain 을 기다리므로 slow 요청(3초)이 끝난 뒤 돌아와야 한다.
	if time.Since(start) < 2*time.Second {
		t.Fatalf("deploy returned too early: %s", time.Since(start))
	}
	wg.Wait()
	if !strings.Contains(slowResp, fmt.Sprintf("pid=%d", p1.Pid())) {
		t.Fatalf("in-flight request should complete on old pid: %q", slowResp)
	}
	if got := get(t, base+"/"); !strings.Contains(got, fmt.Sprintf("pid=%d", p2.Pid())) {
		t.Fatalf("got %q", got)
	}
}

func TestConcurrentDeployRejected(t *testing.T) {
	m, _ := newManager(t, Config{Command: []string{demoBin, "-startup-delay", "1s"}, HealthTimeout: 5 * time.Second})
	errc := make(chan error, 1)
	go func() { _, err := m.Deploy(context.Background()); errc <- err }()
	time.Sleep(200 * time.Millisecond)
	if _, err := m.Deploy(context.Background()); err != ErrDeployInProgress {
		t.Fatalf("expected ErrDeployInProgress, got %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

func TestAutoRestartWhenProcessDies(t *testing.T) {
	m, fwd := newManager(t, Config{Command: []string{demoBin, "-version", "v1"}, AutoRestart: true})
	p1, err := m.Deploy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p1.Cmd.Process.Kill()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cur := m.Current(); cur != nil && cur != p1 && !cur.Exited() && fwd.Backend() == cur.Addr {
			if got := get(t, "http://"+fwd.Addr().String()+"/"); strings.Contains(got, fmt.Sprintf("pid=%d", cur.Pid())) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("process was not restarted")
}
