// Package control 은 유닉스 소켓 위의 작은 HTTP API 로 Registry 를 조작한다.
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
	"portswap/internal/registry"
)

// Server 는 제어 API 서버다.
type Server struct {
	reg  *registry.Registry
	path string
	srv  *http.Server
}

// DeployResult 는 /deploy 응답이다.
type DeployResult struct {
	OK     bool           `json:"ok"`
	Error  string         `json:"error,omitempty"`
	Status manager.Status `json:"status"`
}

// StatusResponse 는 /status 응답이다. 단일 서비스면 status 만, 여러 서비스 전체 조회면 services 맵.
type StatusResponse struct {
	Status   *manager.Status            `json:"status,omitempty"`
	Services map[string]manager.Status  `json:"services,omitempty"`
}

// NewServer 는 소켓 경로에 바인드할 제어 서버를 만든다.
func NewServer(reg *registry.Registry, socketPath string) *Server {
	s := &Server{reg: reg, path: socketPath}
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
	service := r.URL.Query().Get("service")
	if service == "" && s.reg.Count() > 1 && r.URL.Query().Get("all") == "true" {
		writeJSON(w, http.StatusOK, StatusResponse{Services: s.reg.StatusAll()})
		return
	}
	if service == "" && s.reg.Count() > 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "여러 서비스가 있습니다. ?service=이름 또는 ?all=true",
		})
		return
	}
	st, err := s.reg.Status(service)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	service := r.URL.Query().Get("service")
	_, err := s.reg.Deploy(r.Context(), service)
	st, stErr := s.reg.Status(service)
	if stErr != nil && err == nil {
		st = manager.Status{}
	}
	res := DeployResult{OK: err == nil, Status: st}
	code := http.StatusOK
	if err != nil {
		res.Error = err.Error()
		code = http.StatusConflict
		if !errors.Is(err, manager.ErrDeployInProgress) {
			if stErr != nil {
				code = http.StatusNotFound
			} else {
				code = http.StatusBadGateway
			}
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

func statusURL(service string) string {
	if service == "" {
		return "http://portswap/status"
	}
	return "http://portswap/status?service=" + service
}

func deployURL(service string) string {
	if service == "" {
		return "http://portswap/deploy"
	}
	return "http://portswap/deploy?service=" + service
}

// Status 는 현재 상태를 가져온다. service 는 비우면 유일한 서비스일 때만 된다.
func (c *Client) Status(ctx context.Context, service string) (manager.Status, error) {
	var st manager.Status
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, statusURL(service), nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return st, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return st, fmt.Errorf("status %d: %s", resp.StatusCode, body)
	}
	return st, json.NewDecoder(resp.Body).Decode(&st)
}

// StatusAll 은 모든 서비스 상태를 가져온다.
func (c *Client) StatusAll(ctx context.Context) (map[string]manager.Status, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://portswap/status?all=true", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var wrap StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&wrap); err != nil {
		return nil, err
	}
	return wrap.Services, nil
}

// Deploy 는 배포를 요청하고 결과를 돌려준다. 서버가 실패를 보고하면 error 도 함께 돌려준다.
func (c *Client) Deploy(ctx context.Context, service string) (DeployResult, error) {
	var res DeployResult
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, deployURL(service), nil)
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
