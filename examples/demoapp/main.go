// demoapp 은 portswap 시연용 HTTP 서버다. PORT 환경변수(또는 -port)로 리슨하고,
// / 에 자신의 pid 와 버전을 돌려주며 SIGTERM 을 받으면 진행 중인 요청을 마치고 종료한다.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func main() {
	port := flag.Int("port", 0, "리슨 포트 (없으면 PORT 환경변수)")
	version := flag.String("version", os.Getenv("APP_VERSION"), "응답에 표시할 버전")
	startupDelay := flag.Duration("startup-delay", 0, "리슨 시작 전 대기 시간(기동이 느린 앱 흉내)")
	failHealth := flag.Bool("fail-health", false, "헬스체크를 항상 실패시킴(배포 실패 시연)")
	flag.Parse()

	if *port == 0 {
		if p, err := strconv.Atoi(os.Getenv("PORT")); err == nil {
			*port = p
		}
	}
	if *port == 0 {
		log.Fatal("포트가 지정되지 않았습니다")
	}
	time.Sleep(*startupDelay)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if *failHealth {
			http.Error(w, "unhealthy", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
		fmt.Fprintf(w, "slow pid=%d version=%s\n", os.Getpid(), *version)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "pid=%d version=%s\n", os.Getpid(), *version)
	})

	srv := &http.Server{Addr: fmt.Sprintf("127.0.0.1:%d", *port), Handler: mux}
	go func() {
		log.Printf("demoapp pid=%d version=%s listening on %s", os.Getpid(), *version, srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	log.Printf("demoapp pid=%d shutting down", os.Getpid())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}
