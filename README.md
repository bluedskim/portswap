# portswap

외부에 공개된 포트를 고정으로 쥐고 있는 TCP 포워더다. 뒤의 앱 프로세스는 매번 빈 포트에 새로 뜨고, 헬스체크를 통과하면 포워딩이 새 프로세스로 넘어가며 옛 프로세스는 종료된다. 앱 코드를 고칠 필요가 없고 Docker 도 필요 없다.

# 동작 순서

1. `portswap deploy`(또는 serve 프로세스에 SIGHUP)를 받으면 127.0.0.1 의 빈 포트를 하나 고른다.
1. 앱 명령의 `{port}` 자리표시자를 그 포트로 바꾸고 환경변수 `PORT` 도 넣어 새 프로세스를 띄운다.
1. 헬스체크(HTTP 2xx 또는 TCP 연결)가 통과할 때까지 기다린다. 제한 시간 안에 실패하면 새 프로세스만 죽이고 옛 프로세스는 그대로 서비스한다.
1. 통과하면 새 연결의 목적지를 원자적으로 새 포트로 바꾼다. 이미 맺어진 연결은 옛 프로세스에 그대로 남는다.
1. 옛 프로세스로 향한 연결이 모두 끝나거나 `--drain` 시간이 지나면 SIGTERM 을 보내고, `--grace` 안에 안 끝나면 SIGKILL 한다.
1. 현재 프로세스가 스스로 죽으면 같은 절차로 다시 띄운다(`--no-restart` 로 끌 수 있다).

# 빌드

```sh
go build -o portswap .
go build -o examples/demoapp/demoapp ./examples/demoapp   # 시연용 앱
```

# 사용법

```sh
# 기동. '--' 뒤가 앱 명령이다.
portswap serve --listen :8080 \
  --health 'http://127.0.0.1:{port}/health' \
  --drain 30s --grace 15s \
  --control /tmp/portswap.sock \
  -- /opt/app/bin/server --port={port}

# 새 바이너리를 /opt/app/bin/server 에 덮어쓴 뒤
portswap deploy --control /tmp/portswap.sock
# 또는
kill -HUP <serve pid>

portswap status --control /tmp/portswap.sock
```

1. `--health` 를 비우면 포트에 TCP 연결이 되는 것만 확인한다. HTTP 앱이면 헬스 URL 을 주는 편이 안전하다.
1. 앱이 포트를 인자로 못 받으면 환경변수 `PORT` 를 읽게 하면 된다. 둘 다 안 되면 셸로 감싼다. 예: `-- sh -c 'exec java -Dserver.port=$PORT -jar app.jar'`
1. 앱은 SIGTERM 을 받으면 진행 중인 요청을 마치고 종료하도록 되어 있어야 가장 매끄럽다. 그렇지 않아도 `--drain` 이 기존 연결이 빠질 때까지 기다려 준다.
1. 제어 소켓은 0600 권한의 유닉스 소켓이라 같은 사용자만 배포를 호출할 수 있다.

# 시연

```sh
./portswap serve --listen 127.0.0.1:9000 --health 'http://127.0.0.1:{port}/health' \
  -- ./examples/demoapp/demoapp -version v1 &
curl 127.0.0.1:9000/            # pid=... version=v1
./portswap deploy               # 새 pid 로 전환, 옛 pid 종료
curl 127.0.0.1:9000/            # pid 가 바뀌어 있음
```

# 한계

1. L4 포워더라서 클라이언트 IP 가 앱에는 127.0.0.1 로 보인다. 필요하면 PROXY protocol 지원을 추가해야 한다.
1. 앱은 반드시 127.0.0.1 에서 리슨해야 한다. 외부 인터페이스에 직접 바인드하면 포워더를 거치지 않는 경로가 생긴다.
1. 배포는 한 번에 하나만 진행된다. 진행 중에 다시 호출하면 거절된다.

# 테스트

```sh
go test -race ./...
```
