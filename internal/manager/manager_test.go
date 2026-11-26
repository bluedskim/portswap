package manager

import (
	"context"
	"errors"
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

func TestFailedHealthKeepsOldProcess(t *testing.T) {
	m, fwd := newManager(t, Config{Command: []string{demoBin, "-version", "v1"}, HealthTimeout: 700 * time.Millisecond})
	p1, err := m.Deploy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m.cfg.Command = []string{demoBin, "-version", "v2", "-fail-health"}
	_, deployErr := m.Deploy(context.Background())
	if deployErr == nil {
		t.Fatal("expected health failure")
	}
	if !strings.Contains(deployErr.Error(), "stderr") || !strings.Contains(deployErr.Error(), "listening") {
		t.Fatalf("deploy error should include stderr tail: %v", deployErr)
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

func TestConcurrentDeployCoalesces(t *testing.T) {
	m, fwd := newManager(t, Config{Command: []string{demoBin, "-startup-delay", "800ms"}, HealthTimeout: 5 * time.Second})
	baseDeploys, err := m.Deploy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = baseDeploys
	startDeploys := m.Status().Deploys

	errc := make(chan error, 4)
	go func() { _, err := m.Deploy(context.Background()); errc <- err }()
	time.Sleep(100 * time.Millisecond)
	for i := 0; i < 3; i++ {
		go func() { _, err := m.Deploy(context.Background()); errc <- err }()
	}
	for i := 0; i < 4; i++ {
		if err := <-errc; err != nil {
			t.Fatalf("deploy waiter %d: %v", i, err)
		}
	}
	st := m.Status()
	if st.Deploys != startDeploys+2 {
		t.Fatalf("deploys want %d got %d", startDeploys+2, st.Deploys)
	}
	if _, err := http.Get("http://" + fwd.Addr().String() + "/"); err != nil {
		t.Fatal(err)
	}
}

func TestWatchRestartsAfterConcurrentFailedDeploy(t *testing.T) {
	// 진행 중인 배포가 헬스체크에 실패할 때 현재 프로세스가 죽어도, watch 가 배포 종료를 기다린 뒤 재기동해야 한다.
	marker := filepath.Join(t.TempDir(), "watch-race-fail.marker")
	failAware := fmt.Sprintf(
		`if [ -f %q ]; then exec %s -version v2 -fail-health; else exec %s -version v2; fi`,
		marker, demoBin, demoBin,
	)
	m, fwd := newManager(t, Config{
		Command:       []string{demoBin, "-version", "v1"},
		AutoRestart:   true,
		HealthTimeout: 3 * time.Second,
	})
	p1, err := m.Deploy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + fwd.Addr().String()

	m.cfg.Command = []string{"sh", "-c", failAware}
	if err := os.WriteFile(marker, []byte("1"), 0); err != nil {
		t.Fatal(err)
	}
	deployDone := make(chan error, 1)
	go func() {
		_, err := m.Deploy(context.Background())
		deployDone <- err
	}()
	time.Sleep(200 * time.Millisecond)
	p1.Cmd.Process.Kill()

	if deployErr := <-deployDone; deployErr == nil {
		t.Fatal("expected health-check deploy to fail")
	}
	_ = os.Remove(marker)

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		st := m.Status()
		if st.Backend != "" && st.Pid != 0 && st.Pid != p1.Pid() {
			cur := m.Current()
			if cur != nil && !cur.Exited() {
				got := get(t, base+"/")
				if strings.Contains(got, "version=v2") && strings.Contains(got, fmt.Sprintf("pid=%d", cur.Pid())) {
					return
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("service did not recover; backend=%q status=%+v", fwd.Backend(), m.Status())
}

func TestDeployWaitTimeoutStillRunsFollowUp(t *testing.T) {
	m, _ := newManager(t, Config{Command: []string{demoBin, "-startup-delay", "2s"}, HealthTimeout: 5 * time.Second})
	if _, err := m.Deploy(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := m.Status().Deploys
	errc := make(chan error, 1)
	go func() {
		_, err := m.Deploy(context.Background())
		errc <- err
	}()
	time.Sleep(150 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := m.Deploy(ctx)
	if !errors.Is(err, ErrDeployWaitInterrupted) {
		t.Fatalf("expected ErrDeployWaitInterrupted, got %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if m.Status().Deploys >= before+2 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("follow-up deploy did not run; deploys=%d want>=%d", m.Status().Deploys, before+2)
}

func TestShutdownCancelsPendingDeployWaiters(t *testing.T) {
	m, fwd := newManager(t, Config{Command: []string{demoBin, "-startup-delay", "3s"}, HealthTimeout: 5 * time.Second})
	if _, err := m.Deploy(context.Background()); err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = m.Deploy(context.Background()) }()
	time.Sleep(100 * time.Millisecond)
	waitDone := make(chan error, 1)
	go func() {
		_, err := m.Deploy(context.Background())
		waitDone <- err
	}()
	time.Sleep(50 * time.Millisecond)
	m.Shutdown(context.Background())
	select {
	case err := <-waitDone:
		if !errors.Is(err, ErrManagerClosed) && !errors.Is(err, ErrDeployWaitInterrupted) {
			t.Fatalf("waiter: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waiter did not return after shutdown")
	}
	_ = fwd
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
