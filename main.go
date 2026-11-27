// portswap: 외부 포트를 고정으로 쥐고 뒤의 앱 프로세스를 무중단으로 교체하는 TCP 포워더.
//
//	portswap serve  --listen :8080 [옵션] -- <앱 명령> [인자...]
//	portswap serve  --config portswap.yaml
//	portswap deploy [--control 소켓경로] [--service 이름]
//	portswap status [--control 소켓경로] [--service 이름]
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

	"portswap/internal/config"
	"portswap/internal/control"
	"portswap/internal/forwarder"
	"portswap/internal/manager"
	"portswap/internal/registry"
)

const defaultSocket = "/tmp/portswap.sock"

var version = "dev"

func main() {
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			printVersion()
			return
		}
	}
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

func printVersion() {
	fmt.Printf("portswap %s\n", version)
}

func usage() {
	fmt.Fprint(os.Stderr, `사용법:
  portswap version | portswap --version | portswap -v
      빌드에 박힌 버전을 출력합니다.
  portswap serve  --listen :8080 [--health URL] [--health-timeout 30s] [--drain 30s] [--grace 15s]
                  [--control /tmp/portswap.sock] [--no-restart] -- <앱 명령> [인자...]
      앱 명령의 {port} 는 빈 포트로 치환되고 환경변수 PORT 로도 전달됩니다.
      SIGHUP 을 받으면 deploy 와 같은 동작을 합니다(다중 서비스면 모두 배포).
  portswap serve  --config portswap.yaml
      YAML 설정으로 여러 서비스를 한 프로세스에서 기동합니다.
  portswap deploy [--control /tmp/portswap.sock] [--service 이름] [--timeout 5m]
      새 프로세스를 띄워 헬스체크 뒤 포워딩을 전환하고 옛 프로세스를 종료합니다.
      다른 배포가 진행 중이면 끝난 뒤 한 번 더 배포하도록 합쳐지며, 대기 호출은 --timeout 안에 그 결과를 받습니다.
  portswap status [--control /tmp/portswap.sock] [--service 이름] [--all]
`)
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := fs.String("config", "", "여러 서비스를 정의한 YAML 설정 파일")
	listen := fs.String("listen", ":8080", "외부에 공개할 리슨 주소 (단일 서비스 CLI)")
	health := fs.String("health", "", "HTTP 헬스체크 URL 템플릿, 예: http://127.0.0.1:{port}/health (비우면 TCP 연결 확인)")
	healthTimeout := fs.Duration("health-timeout", 30*time.Second, "헬스체크 통과 제한 시간")
	healthInterval := fs.Duration("health-interval", 500*time.Millisecond, "헬스체크 간격")
	drain := fs.Duration("drain", 30*time.Second, "전환 후 옛 프로세스의 기존 연결이 끝나길 기다리는 최대 시간")
	grace := fs.Duration("grace", 15*time.Second, "SIGTERM 후 SIGKILL 까지의 유예")
	sock := fs.String("control", defaultSocket, "제어용 유닉스 소켓 경로")
	noRestart := fs.Bool("no-restart", false, "앱이 스스로 죽어도 재기동하지 않음")
	fs.Parse(args)

	logger := log.New(os.Stderr, "[portswap] ", log.LstdFlags)
	reg := registry.New(logger)
	controlPath := *sock

	if *configPath != "" {
		if err := loadConfigIntoRegistry(*configPath, reg, logger); err != nil {
			return err
		}
		if cfg, err := config.LoadFile(*configPath); err == nil && cfg.Control != "" {
			controlPath = cfg.Control
		}
	} else {
		command := fs.Args()
		if len(command) == 0 {
			return fmt.Errorf("'--config' 또는 '--' 뒤의 앱 명령을 지정하세요")
		}
		mgrCfg := registry.BuildManagerConfig(
			command, *health, *healthTimeout, *healthInterval, *drain, *grace, !*noRestart,
			os.Stdout, os.Stderr, logger,
		)
		if err := addService(reg, "default", *listen, mgrCfg, logger); err != nil {
			return err
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reg.StartForwarders(ctx)

	ctl := control.NewServer(reg, controlPath)
	go func() {
		if err := ctl.ListenAndServe(ctx); err != nil {
			logger.Printf("제어 소켓 오류: %v", err)
		}
	}()
	logger.Printf("제어 소켓 %s", controlPath)

	if err := reg.InitialDeploy(ctx); err != nil {
		cancel()
		sctx, scancel := context.WithTimeout(context.Background(), 30*time.Second)
		reg.Shutdown(sctx)
		scancel()
		return err
	}

	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	for s := range sig {
		if s == syscall.SIGHUP {
			logger.Printf("SIGHUP: 배포 시작")
			go reg.DeployAll(ctx)
			continue
		}
		logger.Printf("%s 수신, 종료합니다", s)
		break
	}
	cancel()
	sctx, scancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer scancel()
	reg.Shutdown(sctx)
	return nil
}

func loadConfigIntoRegistry(path string, reg *registry.Registry, logger *log.Logger) error {
	cfg, err := config.LoadFile(path)
	if err != nil {
		return err
	}
	for name, spec := range cfg.Services {
		spec.ApplyDefaults()
		mgrCfg := registry.BuildManagerConfig(
			spec.Command,
			spec.Health,
			time.Duration(spec.HealthTimeout),
			time.Duration(spec.HealthInterval),
			time.Duration(spec.Drain),
			time.Duration(spec.Grace),
			*spec.AutoRestart,
			os.Stdout, os.Stderr,
			log.New(os.Stderr, fmt.Sprintf("[portswap:%s] ", name), log.LstdFlags),
		)
		if err := addService(reg, name, spec.Listen, mgrCfg, logger); err != nil {
			return fmt.Errorf("서비스 %s: %w", name, err)
		}
	}
	return nil
}

func addService(reg *registry.Registry, name, listen string, mgrCfg manager.Config, logger *log.Logger) error {
	fwd, err := forwarder.New(listen, logger)
	if err != nil {
		return fmt.Errorf("리슨 실패: %w", err)
	}
	logger.Printf("서비스 %s 리슨 %s", name, fwd.Addr())
	mgr := manager.New(mgrCfg, fwd)
	return reg.Add(name, mgr, fwd)
}

func runDeploy(args []string) error {
	fs := flag.NewFlagSet("deploy", flag.ExitOnError)
	sock := fs.String("control", defaultSocket, "제어용 유닉스 소켓 경로")
	service := fs.String("service", "", "배포할 서비스 이름 (유일한 서비스면 생략 가능)")
	timeout := fs.Duration("timeout", 5*time.Minute, "배포·대기 포함 전체 제한 시간")
	fs.Parse(args)
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	res, err := control.NewClient(*sock).Deploy(ctx, *service)
	if err != nil {
		if res.Status.Pid != 0 {
			name := res.Status.Name
			if name != "" {
				fmt.Fprintf(os.Stderr, "서비스 %s: ", name)
			}
			fmt.Fprintf(os.Stderr, "현재 pid %d (포트 %d) 는 그대로 서비스 중입니다\n", res.Status.Pid, res.Status.Port)
		}
		return err
	}
	label := ""
	if res.Status.Name != "" && res.Status.Name != "default" {
		label = fmt.Sprintf("service=%s ", res.Status.Name)
	}
	fmt.Printf("배포 완료: %spid=%d port=%d backend=%s deploys=%d\n", label, res.Status.Pid, res.Status.Port, res.Status.Backend, res.Status.Deploys)
	return nil
}

func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	sock := fs.String("control", defaultSocket, "제어용 유닉스 소켓 경로")
	service := fs.String("service", "", "조회할 서비스 이름 (유일한 서비스면 생략 가능)")
	all := fs.Bool("all", false, "모든 서비스 상태를 JSON 으로 출력")
	fs.Parse(args)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cl := control.NewClient(*sock)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if *all {
		svcs, err := cl.StatusAll(ctx)
		if err != nil {
			return err
		}
		return enc.Encode(svcs)
	}
	st, err := cl.Status(ctx, *service)
	if err != nil {
		return err
	}
	return enc.Encode(st)
}
