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
	Name        string    `json:"name,omitempty"`
	Listen      string    `json:"listen"`
	Backend     string    `json:"backend"`
	Pid         int       `json:"pid"`
	Port        int       `json:"port"`
	StartedAt   time.Time `json:"started_at"`
	Deploys     int       `json:"deploys"`
	ActiveConns int       `json:"active_conns"`
	Deploying   bool      `json:"deploying"`
}

// deployOutcome 은 배포 한 번의 결과다.
type deployOutcome struct {
	p   *proc.Process
	err error
}

// deployWaiter 는 진행 중인 배포가 끝난 뒤 이어질 합쳐진 배포 결과를 기다린다.
type deployWaiter struct {
	ch  chan deployOutcome
	ctx context.Context
}

// Manager 는 현재 앱 프로세스와 포워더를 소유한다.
type Manager struct {
	cfg Config
	fwd *forwarder.Forwarder

	mu            sync.Mutex
	deployMu      sync.Mutex
	deployRunning bool
	pendingFollow bool          // 진행 중 배포 종료 뒤 한 번 더 배포할지
	pendingUser   bool          // 대기 중 호출에 사용자 요청이 포함됐는지
	pendingWatch  *proc.Process // watch 만 대기할 때, 건너뛸지 판단용 죽은 프로세스
	followWaiters []*deployWaiter
	current       *proc.Process
	startedAt     time.Time
	deploys       int
	deploying     bool
	closed        bool
}

// ErrDeployWaitInterrupted 는 배포 대기가 ctx 취소·타임아웃으로 끊겼을 때 돌려준다. 합쳐진 배포는 계속 진행된다.
var ErrDeployWaitInterrupted = errors.New("배포 대기가 중단되었습니다")

// ErrManagerClosed 는 Manager 가 종료된 뒤 배포를 요청했을 때 돌려준다.
var ErrManagerClosed = errors.New("관리자가 종료되었습니다")

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

// Deploy 는 새 프로세스를 띄우고 헬스체크 후 전환한다. 다른 배포가 진행 중이면
// 한 번의 후속 배포로 합쳐지며, 대기한 호출자는 그 후속 배포 결과를 받는다.
func (m *Manager) Deploy(ctx context.Context) (*proc.Process, error) {
	return m.requestDeploy(ctx, true, nil)
}

// deployFromWatch 는 watch 가 죽은 프로세스를 대신해 재기동을 요청한다. 사용자 배포와 겹치면 사용자 배포가 우선한다.
func (m *Manager) deployFromWatch(ctx context.Context, dead *proc.Process) (*proc.Process, error) {
	return m.requestDeploy(ctx, false, dead)
}

func (m *Manager) requestDeploy(ctx context.Context, user bool, dead *proc.Process) (*proc.Process, error) {
	m.deployMu.Lock()
	if m.closed {
		m.deployMu.Unlock()
		return nil, ErrManagerClosed
	}
	if !m.deployRunning {
		m.deployRunning = true
		m.deployMu.Unlock()
		p, err := m.runDeploy(ctx)
		m.finishDeployCycle()
		return p, err
	}
	m.coalesceFollow(user, dead)
	w := &deployWaiter{ch: make(chan deployOutcome, 1), ctx: ctx}
	m.followWaiters = append(m.followWaiters, w)
	m.deployMu.Unlock()

	select {
	case out := <-w.ch:
		return out.p, out.err
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %w", ErrDeployWaitInterrupted, ctx.Err())
	}
}

func (m *Manager) coalesceFollow(user bool, dead *proc.Process) {
	m.pendingFollow = true
	if user {
		m.pendingUser = true
	} else if dead != nil {
		m.pendingWatch = dead
	}
}

// finishDeployCycle 은 진행 중이던 배포가 끝난 뒤, 합쳐진 후속 배포를 최대 한 번씩 반복 실행한다.
func (m *Manager) finishDeployCycle() {
	for {
		m.deployMu.Lock()
		waiters := m.followWaiters
		m.followWaiters = nil
		pending := m.pendingFollow
		pendingUser := m.pendingUser
		watchDead := m.pendingWatch
		m.pendingFollow = false
		m.pendingUser = false
		m.pendingWatch = nil
		closed := m.closed
		m.deployMu.Unlock()

		if !pending {
			m.deployMu.Lock()
			m.deployRunning = false
			m.deployMu.Unlock()
			return
		}
		if closed {
			m.failWaiters(waiters, ErrManagerClosed)
			m.deployMu.Lock()
			m.deployRunning = false
			m.deployMu.Unlock()
			return
		}

		out := m.runFollowDeploy(pendingUser, watchDead)
		m.deliverWaiters(waiters, out)

		m.deployMu.Lock()
		if !m.pendingFollow {
			m.deployRunning = false
			m.deployMu.Unlock()
			return
		}
		m.deployMu.Unlock()
	}
}

func (m *Manager) runFollowDeploy(pendingUser bool, watchDead *proc.Process) deployOutcome {
	if !pendingUser && watchDead != nil {
		m.mu.Lock()
		cur := m.current
		alive := cur != nil && cur != watchDead && !cur.Exited()
		m.mu.Unlock()
		if alive {
			return deployOutcome{p: cur}
		}
	}
	p, err := m.runDeploy(context.Background())
	return deployOutcome{p: p, err: err}
}

func (m *Manager) deliverWaiters(waiters []*deployWaiter, out deployOutcome) {
	for _, w := range waiters {
		select {
		case w.ch <- out:
		default:
		}
	}
}

func (m *Manager) failWaiters(waiters []*deployWaiter, err error) {
	out := deployOutcome{err: err}
	for _, w := range waiters {
		select {
		case w.ch <- out:
		default:
		}
	}
}

// runDeploy 는 실제 배포 한 번을 수행한다.
func (m *Manager) runDeploy(ctx context.Context) (*proc.Process, error) {
	m.setDeploying(true)
	defer m.setDeploying(false)

	port, err := proc.FreePort()
	if err != nil {
		return nil, fmt.Errorf("빈 포트 확보 실패: %w", err)
	}
	m.cfg.Logger.Printf("새 프로세스 기동: 포트 %d", port)
	stderrTail, stderrW := proc.NewStderrTail(m.cfg.Stderr, 0, 0)
	np, err := proc.Start(m.cfg.Command, port, m.cfg.Stdout, stderrW)
	if err != nil {
		return nil, fmt.Errorf("프로세스 기동 실패: %w", err)
	}
	m.cfg.Logger.Printf("pid %d 헬스체크 대기 (최대 %s)", np.Pid(), m.cfg.HealthTimeout)
	if err := proc.WaitHealthy(ctx, np, m.cfg.Health, m.cfg.HealthTimeout, m.cfg.HealthInterval); err != nil {
		err = proc.AppendHealthStderr(err, stderrTail)
		m.cfg.Logger.Printf("pid %d 헬스체크 실패, 새 프로세스 종료: %v", np.Pid(), err)
		np.Stop(context.Background(), m.cfg.Grace)
		return nil, fmt.Errorf("헬스체크 실패: %w", err)
	}

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
		_, err := m.deployFromWatch(context.Background(), p)
		if err == nil {
			return
		}
		if errors.Is(err, ErrManagerClosed) {
			return
		}
		m.cfg.Logger.Printf("재기동 실패: %v (%s 후 재시도)", err, backoff)
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// Shutdown 은 현재 프로세스를 종료한다. 이후 자동 재기동은 하지 않는다.
func (m *Manager) Shutdown(ctx context.Context) {
	m.deployMu.Lock()
	m.closed = true
	waiters := m.followWaiters
	m.followWaiters = nil
	m.pendingFollow = false
	m.deployMu.Unlock()
	m.failWaiters(waiters, ErrManagerClosed)

	m.mu.Lock()
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