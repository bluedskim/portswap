// Package manager 는 포워더와 앱 프로세스를 묶어 무중단 교체(deploy)를 수행한다.
package manager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"portswap/internal/forwarder"
	"portswap/internal/proc"
)

// Config 는 Manager 동작 설정이다.
type Config struct {
	Command        []string // 앱 실행 명령. {port} 자리표시자와 PORT 환경변수로 포트를 전달한다.
	Health         proc.HealthChecker
	HealthTimeout  time.Duration // 헬스체크가 통과해야 하는 제한 시간
	HealthInterval time.Duration
	Drain          time.Duration // 전환 후 옛 프로세스의 기존 연결이 끝나길 기다리는 최대 시간
	Grace          time.Duration // SIGTERM 후 SIGKILL 까지 기다리는 시간
	AutoRestart    bool          // 현재 프로세스가 스스로 죽으면 다시 띄울지
	Stdout, Stderr io.Writer     // 앱 프로세스의 출력
	Logger         *log.Logger
}

// Status 는 현재 상태 스냅샷이다.
type Status struct {
	Listen      string    `json:"listen"`
	Backend     string    `json:"backend"`
	Pid         int       `json:"pid"`
	Port        int       `json:"port"`
	StartedAt   time.Time `json:"started_at"`
	Deploys     int       `json:"deploys"`
	ActiveConns int       `json:"active_conns"`
	Deploying   bool      `json:"deploying"`
}

// Manager 는 현재 앱 프로세스와 포워더를 소유한다.
type Manager struct {
	cfg Config
	fwd *forwarder.Forwarder

	mu        sync.Mutex
	deployMu  sync.Mutex // deploy 는 한 번에 하나만
	current   *proc.Process
	startedAt time.Time
	deploys   int
	deploying bool
	closed    bool
}

// ErrDeployInProgress 는 다른 deploy 가 진행 중일 때 돌려준다.
var ErrDeployInProgress = errors.New("다른 배포가 진행 중입니다")

// New 는 Manager 를 만든다. 아직 프로세스는 띄우지 않는다.
func New(cfg Config, fwd *forwarder.Forwarder) *Manager {
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if cfg.Health == nil {
		cfg.Health = proc.TCPHealth{}
	}
	if cfg.HealthInterval == 0 {
		cfg.HealthInterval = 500 * time.Millisecond
	}
	if cfg.HealthTimeout == 0 {
		cfg.HealthTimeout = 30 * time.Second
	}
	if cfg.Grace == 0 {
		cfg.Grace = 15 * time.Second
	}
	if cfg.Stdout == nil {
		cfg.Stdout = io.Discard
	}
	if cfg.Stderr == nil {
		cfg.Stderr = io.Discard
	}
	return &Manager{cfg: cfg, fwd: fwd}
}

// Current 는 현재 서비스 중인 프로세스를 돌려준다(없으면 nil).
func (m *Manager) Current() *proc.Process {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current
}

// Status 는 상태 스냅샷을 돌려준다.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Status{Listen: m.fwd.Addr().String(), Backend: m.fwd.Backend(), Deploys: m.deploys, StartedAt: m.startedAt, Deploying: m.deploying}
	if m.current != nil {
		s.Pid = m.current.Pid()
		s.Port = m.current.Port
		s.ActiveConns = m.fwd.ActiveConns(m.current.Addr)
	}
	return s
}

// Deploy 는 새 프로세스를 빈 포트에 띄우고 헬스체크를 통과하면 포워딩을 전환한 뒤 옛 프로세스를 종료한다.
// 헬스체크에 실패하면 새 프로세스를 죽이고 옛 프로세스는 그대로 둔다.
func (m *Manager) Deploy(ctx context.Context) (*proc.Process, error) {
	if !m.deployMu.TryLock() {
		return nil, ErrDeployInProgress
	}
	defer m.deployMu.Unlock()
	m.setDeploying(true)
	defer m.setDeploying(false)

	port, err := proc.FreePort()
	if err != nil {
		return nil, fmt.Errorf("빈 포트 확보 실패: %w", err)
	}
	m.cfg.Logger.Printf("새 프로세스 기동: 포트 %d", port)
	np, err := proc.Start(m.cfg.Command, port, m.cfg.Stdout, m.cfg.Stderr)
	if err != nil {
		return nil, fmt.Errorf("프로세스 기동 실패: %w", err)
	}
	m.cfg.Logger.Printf("pid %d 헬스체크 대기 (최대 %s)", np.Pid(), m.cfg.HealthTimeout)
	if err := proc.WaitHealthy(ctx, np, m.cfg.Health, m.cfg.HealthTimeout, m.cfg.HealthInterval); err != nil {
		m.cfg.Logger.Printf("pid %d 헬스체크 실패, 새 프로세스 종료: %v", np.Pid(), err)
		np.Stop(context.Background(), m.cfg.Grace)
		return nil, fmt.Errorf("헬스체크 실패: %w", err)
	}

	// 전환
	m.mu.Lock()
	old := m.current
	m.current = np
	m.startedAt = time.Now()
	m.deploys++
	m.fwd.SetBackend(np.Addr)
	m.mu.Unlock()
	m.cfg.Logger.Printf("포워딩 전환 완료: %s (pid %d)", np.Addr, np.Pid())

	if m.cfg.AutoRestart {
		go m.watch(np)
	}
	if old != nil {
		m.retire(old)
	}
	return np, nil
}

// retire 는 옛 프로세스의 기존 연결이 빠지길 기다린 뒤 종료한다.
func (m *Manager) retire(old *proc.Process) {
	if m.cfg.Drain > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), m.cfg.Drain)
		drained := m.fwd.WaitDrained(ctx, old.Addr)
		cancel()
		if !drained {
			m.cfg.Logger.Printf("pid %d: %s 안에 연결 %d개가 남아 있어 그대로 종료합니다", old.Pid(), m.cfg.Drain, m.fwd.ActiveConns(old.Addr))
		}
	}
	m.cfg.Logger.Printf("옛 프로세스 pid %d 종료 (SIGTERM, 유예 %s)", old.Pid(), m.cfg.Grace)
	old.Stop(context.Background(), m.cfg.Grace)
	m.cfg.Logger.Printf("옛 프로세스 pid %d 종료됨", old.Pid())
}

// watch 는 현재 프로세스가 스스로 죽으면 재기동을 시도한다.
func (m *Manager) watch(p *proc.Process) {
	<-p.Done()
	m.mu.Lock()
	isCurrent := m.current == p && !m.closed
	if isCurrent {
		m.fwd.SetBackend("")
	}
	m.mu.Unlock()
	if !isCurrent {
		return
	}
	m.cfg.Logger.Printf("현재 프로세스 pid %d 가 예기치 않게 종료됨, 재기동 시도", p.Pid())
	backoff := time.Second
	for {
		m.mu.Lock()
		closed := m.closed
		m.mu.Unlock()
		if closed {
			return
		}
		if _, err := m.Deploy(context.Background()); err == nil {
			return
		} else if !errors.Is(err, ErrDeployInProgress) {
			m.cfg.Logger.Printf("재기동 실패: %v (%s 후 재시도)", err, backoff)
		} else {
			return
		}
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// Shutdown 은 현재 프로세스를 종료한다. 이후 자동 재기동은 하지 않는다.
func (m *Manager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	m.closed = true
	cur := m.current
	m.current = nil
	m.fwd.SetBackend("")
	m.mu.Unlock()
	if cur != nil {
		cur.Stop(ctx, m.cfg.Grace)
	}
}

func (m *Manager) setDeploying(v bool) {
	m.mu.Lock()
	m.deploying = v
	m.mu.Unlock()
}
