package proc

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuf 는 자식 프로세스 출력 goroutine 과 테스트가 동시에 접근해도 안전한 버퍼다.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestExpand(t *testing.T) {
	got := Expand([]string{"app", "--port={port}", "-p", "{port}", "x"}, 8080)
	want := []string{"app", "--port=8080", "-p", "8080", "x"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("got %v", got)
	}
}

func TestFreePortIsBindable(t *testing.T) {
	p, err := FreePort()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
}

func TestStartSetsPortEnvAndStop(t *testing.T) {
	// 셸로 PORT 환경변수를 출력하고 대기하는 프로세스를 띄운다.
	var out syncBuf
	p, err := Start([]string{"sh", "-c", "echo PORT=$PORT ARG={port}; sleep 30"}, 12345, &out, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if !strings.Contains(out.String(), "PORT=12345 ARG=12345") {
		t.Fatalf("stdout=%q", out.String())
	}
	start := time.Now()
	p.Stop(context.Background(), 2*time.Second)
	if !p.Exited() {
		t.Fatal("not exited")
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatal("SIGTERM should have ended sh quickly")
	}
}

func TestStopKillsAfterGrace(t *testing.T) {
	// SIGTERM 을 무시하는 프로세스는 grace 뒤에 SIGKILL 되어야 한다.
	p, err := Start([]string{"sh", "-c", "trap '' TERM; sleep 30"}, 1, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	p.Stop(context.Background(), 300*time.Millisecond)
	el := time.Since(start)
	if el < 250*time.Millisecond || el > 2*time.Second {
		t.Fatalf("elapsed %s", el)
	}
}

func TestWaitHealthyTCP(t *testing.T) {
	port, _ := FreePort()
	// 0.3초 뒤에 리슨을 시작하는 프로세스
	p, err := Start([]string{"sh", "-c", "sleep 0.3; exec python3 -c 'import socket,os,time;s=socket.socket();s.bind((\"127.0.0.1\",int(os.environ[\"PORT\"])));s.listen();time.sleep(30)'"}, port, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop(context.Background(), time.Second)
	if err := WaitHealthy(context.Background(), p, TCPHealth{}, 5*time.Second, 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
}

func TestWaitHealthyFailsWhenProcessDies(t *testing.T) {
	p, err := Start([]string{"sh", "-c", "exit 3"}, 1, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	err = WaitHealthy(context.Background(), p, TCPHealth{}, 5*time.Second, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "종료") {
		t.Fatalf("err=%v", err)
	}
}

func TestWaitHealthyTimeout(t *testing.T) {
	p, err := Start([]string{"sleep", "30"}, 1, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop(context.Background(), time.Second)
	err = WaitHealthy(context.Background(), p, TCPHealth{}, 300*time.Millisecond, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "시간 초과") {
		t.Fatalf("err=%v", err)
	}
}

func TestHTTPHealth(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(503)
	}))
	defer ok.Close()
	port := ok.Listener.Addr().(*net.TCPAddr).Port

	h := HTTPHealth{URLTemplate: "http://127.0.0.1:{port}/health"}
	if err := h.Check(context.Background(), port); err != nil {
		t.Fatal(err)
	}
	bad := HTTPHealth{URLTemplate: "http://127.0.0.1:{port}/nope"}
	if err := bad.Check(context.Background(), port); err == nil {
		t.Fatal("expected 503 error")
	}
	if _, isTCP := NewHealthChecker("").(TCPHealth); !isTCP {
		t.Fatal("empty spec should be TCP")
	}
}
