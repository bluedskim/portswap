// Package control 은 유닉스 소켓 위의 작은 HTTP API 로 Manager 를 조작한다.
package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"portswap/internal/manager"
)

// Server 는 제어 API 서버다.
type Server struct {
	mgr  *manager.Manager
	path string
	srv  *http.Server
}

// DeployResult 는 /deploy 응답이다.
type DeployResult struct {
	OK     bool           `json:"ok"`
	Error  string         `json:"error,omitempty"`
	Status manager.Status `json:"status"`
}

// NewServer 는 소켓 경로에 바인드할 제어 서버를 만든다.
func NewServer(mgr *manager.Manager, socketPath string) *Server {
	s := &Server{mgr: mgr, path: socketPath}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("POST /deploy", s.handleDeploy)
	s.srv = &http.Server{Handler: mux}
	return s
}

// Handler 는 테스트용으로 HTTP 핸들러를 노출한다.
func (s *Server) Handler() http.Handler { return s.srv.Handler }

// ListenAndServe 는 소켓을 만들고 ctx 가 끝날 때까지 서비스한다. 기존 소켓 파일은 지운다.
func (s *Server) ListenAndServe(ctx context.Context) error {
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ln, err := net.Listen("unix", s.path)
	if err != nil {
		return err
	}
	os.Chmod(s.path, 0o600)
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s.srv.Shutdown(sctx)
		os.Remove(s.path)
	}()
	if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.mgr.Status())
}

func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	_, err := s.mgr.Deploy(r.Context())
	res := DeployResult{OK: err == nil, Status: s.mgr.Status()}
	code := http.StatusOK
	if err != nil {
		res.Error = err.Error()
		code = http.StatusConflict
		if !errors.Is(err, manager.ErrDeployInProgress) {
			code = http.StatusBadGateway
		}
	}
	writeJSON(w, code, res)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// Client 는 유닉스 소켓으로 제어 API 를 호출한다.
type Client struct {
	http *http.Client
}

// NewClient 는 socketPath 에 연결하는 클라이언트를 만든다.
func NewClient(socketPath string) *Client {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socketPath)
	}}
	return &Client{http: &http.Client{Transport: tr}}
}

// Status 는 현재 상태를 가져온다.
func (c *Client) Status(ctx context.Context) (manager.Status, error) {
	var st manager.Status
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://portswap/status", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return st, err
	}
	defer resp.Body.Close()
	return st, json.NewDecoder(resp.Body).Decode(&st)
}

// Deploy 는 배포를 요청하고 결과를 돌려준다. 서버가 실패를 보고하면 error 도 함께 돌려준다.
func (c *Client) Deploy(ctx context.Context) (DeployResult, error) {
	var res DeployResult
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://portswap/deploy", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return res, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &res); err != nil {
		return res, fmt.Errorf("응답 해석 실패(%d): %s", resp.StatusCode, body)
	}
	if !res.OK {
		return res, errors.New(res.Error)
	}
	return res, nil
}
