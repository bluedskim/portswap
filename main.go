// portswap: 외부 포트를 고정으로 쥐고 뒤의 앱 프로세스를 무중단으로 교체하는 TCP 포워더.
//
//	portswap serve  --listen :8080 [옵션] -- <앱 명령> [인자...]
//	portswap deploy [--control 소켓경로]
//	portswap status [--control 소켓경로]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"portswap/internal/control"
	"portswap/internal/forwarder"
	"portswap/internal/manager"
	"portswap/internal/proc"
)

const defaultSocket = "/tmp/portswap.sock"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = runServe(os.Args[2:])
	case "deploy":
		err = runDeploy(os.Args[2:])
	case "status":
		err = runStatus(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "오류:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `사용법:
  portswap serve  --listen :8080 [--health URL] [--health-timeout 30s] [--drain 30s] [--grace 15s]
                  [--control /tmp/portswap.sock] [--no-restart] -- <앱 명령> [인자...]
      앱 명령의 {port} 는 빈 포트로 치환되고 환경변수 PORT 로도 전달됩니다.
      SIGHUP 을 받으면 deploy 와 같은 동작을 합니다.
  portswap deploy [--control /tmp/portswap.sock]
      새 프로세스를 띄워 헬스체크 뒤 포워딩을 전환하고 옛 프로세스를 종료합니다.
  portswap status [--control /tmp/portswap.sock]
`)
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", ":8080", "외부에 공개할 리슨 주소")
	health := fs.String("health", "", "HTTP 헬스체크 URL 템플릿, 예: http://127.0.0.1:{port}/health (비우면 TCP 연결 확인)")
	healthTimeout := fs.Duration("health-timeout", 30*time.Second, "헬스체크 통과 제한 시간")
	healthInterval := fs.Duration("health-interval", 500*time.Millisecond, "헬스체크 간격")
	drain := fs.Duration("drain", 30*time.Second, "전환 후 옛 프로세스의 기존 연결이 끝나길 기다리는 최대 시간")
	grace := fs.Duration("grace", 15*time.Second, "SIGTERM 후 SIGKILL 까지의 유예")
	sock := fs.String("control", defaultSocket, "제어용 유닉스 소켓 경로")
	noRestart := fs.Bool("no-restart", false, "앱이 스스로 죽어도 재기동하지 않음")
	fs.Parse(args)
	command := fs.Args()
	if len(command) == 0 {
		return fmt.Errorf("'--' 뒤에 앱 명령을 지정하세요")
	}

	logger := log.New(os.Stderr, "[portswap] ", log.LstdFlags)
	fwd, err := forwarder.New(*listen, logger)
	if err != nil {
		return fmt.Errorf("리슨 실패: %w", err)
	}
	logger.Printf("리슨 %s", fwd.Addr())

	mgr := manager.New(manager.Config{
		Command:        command,
		Health:         proc.NewHealthChecker(*health),
		HealthTimeout:  *healthTimeout,
		HealthInterval: *healthInterval,
		Drain:          *drain,
		Grace:          *grace,
		AutoRestart:    !*noRestart,
		Stdout:         os.Stdout,
		Stderr:         os.Stderr,
		Logger:         logger,
	}, fwd)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		if err := fwd.Serve(ctx); err != nil {
			logger.Printf("포워더 종료: %v", err)
		}
	}()
	ctl := control.NewServer(mgr, *sock)
	go func() {
		if err := ctl.ListenAndServe(ctx); err != nil {
			logger.Printf("제어 소켓 오류: %v", err)
		}
	}()
	logger.Printf("제어 소켓 %s", *sock)

	if _, err := mgr.Deploy(ctx); err != nil {
		cancel()
		return fmt.Errorf("초기 기동 실패: %w", err)
	}

	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	for s := range sig {
		if s == syscall.SIGHUP {
			logger.Printf("SIGHUP: 배포 시작")
			go func() {
				if _, err := mgr.Deploy(ctx); err != nil {
					logger.Printf("배포 실패: %v", err)
				}
			}()
			continue
		}
		logger.Printf("%s 수신, 종료합니다", s)
		break
	}
	cancel()
	sctx, scancel := context.WithTimeout(context.Background(), *grace+2*time.Second)
	defer scancel()
	mgr.Shutdown(sctx)
	return nil
}

func runDeploy(args []string) error {
	fs := flag.NewFlagSet("deploy", flag.ExitOnError)
	sock := fs.String("control", defaultSocket, "제어용 유닉스 소켓 경로")
	timeout := fs.Duration("timeout", 5*time.Minute, "배포 완료 대기 시간")
	fs.Parse(args)
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	res, err := control.NewClient(*sock).Deploy(ctx)
	if err != nil {
		if res.Status.Pid != 0 {
			fmt.Fprintf(os.Stderr, "현재 pid %d (포트 %d) 는 그대로 서비스 중입니다\n", res.Status.Pid, res.Status.Port)
		}
		return err
	}
	fmt.Printf("배포 완료: pid=%d port=%d backend=%s deploys=%d\n", res.Status.Pid, res.Status.Port, res.Status.Backend, res.Status.Deploys)
	return nil
}

func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	sock := fs.String("control", defaultSocket, "제어용 유닉스 소켓 경로")
	fs.Parse(args)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := control.NewClient(*sock).Status(ctx)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(st)
}
