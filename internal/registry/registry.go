// Package registry 는 여러 독립 서비스(포워더+매니저)를 한 프로세스에서 관리한다.
package registry

import (
	"context"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"portswap/internal/forwarder"
	"portswap/internal/manager"
	"portswap/internal/proc"
)

// Entry 는 이름이 붙은 서비스 인스턴스다.
type Entry struct {
	Name string
	Mgr  *manager.Manager
	Fwd  *forwarder.Forwarder
}

// Registry 는 서비스 이름으로 매니저를 조회한다.
type Registry struct {
	mu       sync.RWMutex
	entries  map[string]*Entry
	order    []string // 등록 순서
	logger   *log.Logger
}

// New 는 빈 Registry 를 만든다.
func New(logger *log.Logger) *Registry {
	if logger == nil {
		logger = log.Default()
	}
	return &Registry{entries: map[string]*Entry{}, logger: logger}
}

// Add 는 서비스를 등록한다. 같은 이름이 있으면 오류다.
func (r *Registry) Add(name string, mgr *manager.Manager, fwd *forwarder.Forwarder) error {
	if name == "" {
		return fmt.Errorf("서비스 이름이 비어 있습니다")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries[name] != nil {
		return fmt.Errorf("서비스 %q 가 이미 있습니다", name)
	}
	r.entries[name] = &Entry{Name: name, Mgr: mgr, Fwd: fwd}
	r.order = append(r.order, name)
	return nil
}

// Names 은 등록된 서비스 이름 목록을 돌려준다.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Count 는 등록된 서비스 수다.
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}

// ResolveName 은 deploy/status 에 쓸 서비스 이름을 고른다. 비어 있으면 유일한 서비스일 때만 허용한다.
func (r *Registry) ResolveName(name string) (string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if name != "" {
		if r.entries[name] == nil {
			return "", fmt.Errorf("알 수 없는 서비스 %q", name)
		}
		return name, nil
	}
	if len(r.entries) == 1 {
		return r.order[0], nil
	}
	if len(r.entries) == 0 {
		return "", fmt.Errorf("등록된 서비스가 없습니다")
	}
	return "", fmt.Errorf("서비스 이름을 지정하세요 (--service). 사용 가능: %v", r.order)
}

// EntryFor 는 이름에 해당하는 Entry 를 돌려준다.
func (r *Registry) EntryFor(name string) (*Entry, error) {
	resolved, err := r.ResolveName(name)
	if err != nil {
		return nil, err
	}
	r.mu.RLock()
	e := r.entries[resolved]
	r.mu.RUnlock()
	return e, nil
}

// Deploy 는 지정 서비스에 배포를 수행한다.
func (r *Registry) Deploy(ctx context.Context, name string) (*proc.Process, error) {
	e, err := r.EntryFor(name)
	if err != nil {
		return nil, err
	}
	return e.Mgr.Deploy(ctx)
}

// Status 는 지정 서비스 상태를 돌려준다.
func (r *Registry) Status(name string) (manager.Status, error) {
	e, err := r.EntryFor(name)
	if err != nil {
		return manager.Status{}, err
	}
	st := e.Mgr.Status()
	st.Name = e.Name
	return st, nil
}

// StatusAll 은 모든 서비스 상태를 이름 키로 돌려준다.
func (r *Registry) StatusAll() map[string]manager.Status {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]manager.Status, len(r.entries))
	for _, name := range r.order {
		st := r.entries[name].Mgr.Status()
		st.Name = name
		out[name] = st
	}
	return out
}

// DeployAll 은 모든 서비스에 배포를 병렬로 시도한다. 오류는 로그에 남긴다.
func (r *Registry) DeployAll(ctx context.Context) {
	names := r.Names()
	var wg sync.WaitGroup
	for _, name := range names {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			if _, err := r.Deploy(ctx, n); err != nil {
				r.logger.Printf("서비스 %s 배포 실패: %v", n, err)
			}
		}(name)
	}
	wg.Wait()
}

// StartForwarders 는 모든 포워더를 ctx 동안 서비스한다.
func (r *Registry) StartForwarders(ctx context.Context) {
	r.mu.RLock()
	entries := make([]*Entry, len(r.order))
	for i, name := range r.order {
		entries[i] = r.entries[name]
	}
	r.mu.RUnlock()
	for _, e := range entries {
		go func(fwd *forwarder.Forwarder) {
			if err := fwd.Serve(ctx); err != nil {
				r.logger.Printf("포워더 종료: %v", err)
			}
		}(e.Fwd)
	}
}

// InitialDeploy 는 모든 서비스를 처음 띄운다. 하나라도 실패하면 오류다.
func (r *Registry) InitialDeploy(ctx context.Context) error {
	for _, name := range r.Names() {
		if _, err := r.Deploy(ctx, name); err != nil {
			return fmt.Errorf("서비스 %s 초기 기동 실패: %w", name, err)
		}
	}
	return nil
}

// Shutdown 은 모든 서비스를 종료한다.
func (r *Registry) Shutdown(ctx context.Context) {
	r.mu.RLock()
	entries := make([]*Entry, len(r.order))
	for i, name := range r.order {
		entries[i] = r.entries[name]
	}
	r.mu.RUnlock()
	for _, e := range entries {
		e.Mgr.Shutdown(ctx)
	}
}

// BuildManagerConfig 는 ServiceSpec 필드로 manager.Config 를 만든다.
func BuildManagerConfig(command []string, health string, healthTimeout, healthInterval, drain, grace time.Duration, autoRestart bool, stdout, stderr io.Writer, logger *log.Logger) manager.Config {
	return manager.Config{
		Command:        command,
		Health:         proc.NewHealthChecker(health),
		HealthTimeout:  healthTimeout,
		HealthInterval: healthInterval,
		Drain:          drain,
		Grace:          grace,
		AutoRestart:    autoRestart,
		Stdout:         stdout,
		Stderr:         stderr,
		Logger:         logger,
	}
}
