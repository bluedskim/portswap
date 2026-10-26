// Package proc 는 앱 프로세스의 기동, 헬스체크, 종료를 담당한다.
package proc

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// FreePort 는 127.0.0.1 에서 현재 비어 있는 TCP 포트를 찾아 돌려준다.
func FreePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// Expand 는 인자 안의 {port} 자리표시자를 실제 포트로 바꾼다.
func Expand(args []string, port int) []string {
	out := make([]string, len(args))
	p := strconv.Itoa(port)
	for i, a := range args {
		out[i] = strings.ReplaceAll(a, "{port}", p)
	}
	return out
}

// Process 는 기동된 앱 프로세스 하나를 나타낸다.
type Process struct {
	Cmd  *exec.Cmd
	Port int
	Addr string
	done chan struct{}
	err  error
}

// Start 는 command 를 port 로 기동한다. 인자의 {port} 는 치환되고 환경변수 PORT 도 설정된다.
// 자식 프로세스는 별도 프로세스 그룹으로 띄워 부모의 시그널에 휩쓸리지 않게 한다.
func Start(command []string, port int, stdout, stderr io.Writer) (*Process, error) {
	if len(command) == 0 {
		return nil, fmt.Errorf("실행할 명령이 없습니다")
	}
	args := Expand(command, port)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = append(os.Environ(), "PORT="+strconv.Itoa(port))
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &Process{Cmd: cmd, Port: port, Addr: fmt.Sprintf("127.0.0.1:%d", port), done: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

// Pid 는 프로세스 ID 를 돌려준다.
func (p *Process) Pid() int { return p.Cmd.Process.Pid }

// Done 은 프로세스가 끝나면 닫히는 채널이다.
func (p *Process) Done() <-chan struct{} { return p.done }

// Exited 는 프로세스가 이미 끝났는지 알려준다.
func (p *Process) Exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// Stop 은 SIGTERM 을 보내고 grace 동안 기다린 뒤에도 살아 있으면 SIGKILL 한다.
// 종료 대기 중 ctx 가 끝나면 즉시 SIGKILL 한다.
func (p *Process) Stop(ctx context.Context, grace time.Duration) {
	if p.Exited() {
		return
	}
	_ = syscall.Kill(-p.Pid(), syscall.SIGTERM)
	t := time.NewTimer(grace)
	defer t.Stop()
	select {
	case <-p.done:
		return
	case <-t.C:
	case <-ctx.Done():
	}
	_ = syscall.Kill(-p.Pid(), syscall.SIGKILL)
	<-p.done
}

// HealthChecker 는 백엔드가 요청을 받을 준비가 되었는지 확인한다.
type HealthChecker interface {
	Check(ctx context.Context, port int) error
}

// TCPHealth 는 포트에 TCP 연결이 되면 정상으로 본다.
type TCPHealth struct{}

func (TCPHealth) Check(ctx context.Context, port int) error {
	d := net.Dialer{Timeout: time.Second}
	c, err := d.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	return c.Close()
}

// HTTPHealth 는 URL 템플릿({port} 치환)에 GET 을 보내 2xx 이면 정상으로 본다.
type HTTPHealth struct {
	URLTemplate string
	Client      *http.Client
}

func (h HTTPHealth) Check(ctx context.Context, port int) error {
	cl := h.Client
	if cl == nil {
		cl = &http.Client{Timeout: 2 * time.Second}
	}
	url := strings.ReplaceAll(h.URLTemplate, "{port}", strconv.Itoa(port))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("헬스체크 응답 %d", resp.StatusCode)
	}
	return nil
}

// NewHealthChecker 는 spec 이 비어 있으면 TCP, 아니면 HTTP 헬스체커를 만든다.
func NewHealthChecker(spec string) HealthChecker {
	if spec == "" {
		return TCPHealth{}
	}
	return HTTPHealth{URLTemplate: spec}
}

// WaitHealthy 는 프로세스가 정상 응답할 때까지 interval 간격으로 헬스체크를 반복한다.
// timeout 안에 성공하지 못하거나 프로세스가 먼저 죽으면 오류를 돌려준다.
func WaitHealthy(ctx context.Context, p *Process, hc HealthChecker, timeout, interval time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var last error
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if p.Exited() {
			return fmt.Errorf("프로세스가 헬스체크 전에 종료됨: %v", p.err)
		}
		if last = hc.Check(ctx, p.Port); last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("헬스체크 시간 초과(%s): %v", timeout, last)
		case <-p.done:
			return fmt.Errorf("프로세스가 헬스체크 전에 종료됨: %v", p.err)
		case <-t.C:
		}
	}
}
