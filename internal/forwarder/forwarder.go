// Package forwarder 는 외부 포트로 들어온 TCP 연결을 현재 백엔드 주소로 전달한다.
// 백엔드 주소는 무중단으로 교체할 수 있으며, 이미 맺어진 연결은 기존 백엔드에 그대로 유지된다.
package forwarder

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Forwarder 는 하나의 리스닝 포트와 교체 가능한 백엔드 주소를 가진 L4 포워더다.
type Forwarder struct {
	ln          net.Listener
	backend     atomic.Pointer[string]
	dialTimeout time.Duration
	logger      *log.Logger

	mu     sync.Mutex
	active map[string]int // 백엔드 주소별 현재 연결 수
	wg     sync.WaitGroup
}

// New 는 listen 주소에서 리스닝을 시작한다. 백엔드는 비어 있을 수 있으며, 그 경우 연결은 즉시 닫힌다.
func New(listen string, logger *log.Logger) (*Forwarder, error) {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = log.Default()
	}
	f := &Forwarder{ln: ln, dialTimeout: 5 * time.Second, logger: logger, active: map[string]int{}}
	return f, nil
}

// Addr 는 실제로 바인드된 리스닝 주소를 돌려준다.
func (f *Forwarder) Addr() net.Addr { return f.ln.Addr() }

// SetBackend 는 새 연결이 향할 백엔드 주소를 원자적으로 바꾼다. 빈 문자열이면 새 연결을 거부한다.
func (f *Forwarder) SetBackend(addr string) {
	f.backend.Store(&addr)
}

// Backend 는 현재 백엔드 주소를 돌려준다.
func (f *Forwarder) Backend() string {
	p := f.backend.Load()
	if p == nil {
		return ""
	}
	return *p
}

// ActiveConns 는 특정 백엔드로 열려 있는 연결 수를 돌려준다.
func (f *Forwarder) ActiveConns(backend string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active[backend]
}

// WaitDrained 는 backend 로 향한 연결이 모두 끝나거나 ctx 가 끝날 때까지 기다린다.
// 모두 끝났으면 true 를 돌려준다.
func (f *Forwarder) WaitDrained(ctx context.Context, backend string) bool {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		if f.ActiveConns(backend) == 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return f.ActiveConns(backend) == 0
		case <-t.C:
		}
	}
}

// Serve 는 ctx 가 끝날 때까지 연결을 받아 전달한다.
func (f *Forwarder) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		f.ln.Close()
	}()
	for {
		c, err := f.ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				f.wg.Wait()
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			f.logger.Printf("accept 오류: %v", err)
			continue
		}
		f.wg.Add(1)
		go f.handle(c)
	}
}

func (f *Forwarder) handle(client net.Conn) {
	defer f.wg.Done()
	defer client.Close()

	backend := f.Backend()
	if backend == "" {
		return
	}
	up, err := net.DialTimeout("tcp", backend, f.dialTimeout)
	if err != nil {
		f.logger.Printf("백엔드 %s 연결 실패: %v", backend, err)
		return
	}
	defer up.Close()

	f.track(backend, +1)
	defer f.track(backend, -1)

	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
		done <- struct{}{}
	}
	go pipe(up, client)
	go pipe(client, up)
	<-done
	<-done
}

func (f *Forwarder) track(backend string, delta int) {
	f.mu.Lock()
	f.active[backend] += delta
	if f.active[backend] <= 0 {
		delete(f.active, backend)
	}
	f.mu.Unlock()
}
