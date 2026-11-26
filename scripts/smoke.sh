#!/bin/sh
# Process-level smoke test for portswap (multi + single CLI). Run from repo root.
set -e
cd "$(dirname "$0")/.."
ROOT="$PWD"
LOG_MULTI=/tmp/portswap-smoke-multi.log
LOG_SINGLE=/tmp/portswap-smoke-single.log
CONTROL=/tmp/portswap.sock
CONTROL_SINGLE=/tmp/portswap-single.sock

cleanup() {
  if [ -n "$PID_MULTI" ] && kill -0 "$PID_MULTI" 2>/dev/null; then
    kill -TERM "$PID_MULTI" 2>/dev/null || true
    wait "$PID_MULTI" 2>/dev/null || true
  fi
  if [ -n "$PID_SINGLE" ] && kill -0 "$PID_SINGLE" 2>/dev/null; then
    kill -TERM "$PID_SINGLE" 2>/dev/null || true
    wait "$PID_SINGLE" 2>/dev/null || true
  fi
  rm -f "$CONTROL" "$CONTROL_SINGLE"
}
trap cleanup EXIT

go build -o portswap .
go build -o examples/demoapp/demoapp ./examples/demoapp
./examples/javademo/build.sh

rm -f "$CONTROL" "$CONTROL_SINGLE"
./portswap serve --config examples/portswap-multiservice.yaml >"$LOG_MULTI" 2>&1 &
PID_MULTI=$!
for i in $(seq 1 60); do
  if curl -sf --max-time 1 http://127.0.0.1:9000/health >/dev/null 2>&1 && \
     curl -sf --max-time 1 http://127.0.0.1:9001/health >/dev/null 2>&1; then
    break
  fi
  if ! kill -0 "$PID_MULTI" 2>/dev/null; then
    echo "multi serve exited early; log:"
    cat "$LOG_MULTI"
    exit 1
  fi
  sleep 0.25
done

WEB1=$(curl -sS http://127.0.0.1:9000/)
API1=$(curl -sS http://127.0.0.1:9001/)
echo "multi initial web=$WEB1"
echo "multi initial api=$API1"
case "$WEB1" in *pid=*version=v1*) ;; *) echo "unexpected web body"; exit 1;; esac
case "$API1" in *pid=*version=v1*) ;; *) echo "unexpected api body"; exit 1;; esac

./portswap deploy --control "$CONTROL" --service api
API2=$(curl -sS http://127.0.0.1:9001/)
echo "multi after api deploy=$API2"
case "$API2" in *pid=*version=v1*) ;; *) echo "api deploy broke body"; exit 1;; esac
PID_API1=$(echo "$API1" | sed -n 's/.*pid=\([0-9]*\).*/\1/p')
PID_API2=$(echo "$API2" | sed -n 's/.*pid=\([0-9]*\).*/\1/p')
if [ "$PID_API1" = "$PID_API2" ]; then
  echo "api pid should change after deploy ($PID_API1 -> $PID_API2)"
  exit 1
fi

./portswap deploy --control "$CONTROL" --service web
WEB2=$(curl -sS http://127.0.0.1:9000/)
echo "multi after web deploy=$WEB2"
PID_WEB1=$(echo "$WEB1" | sed -n 's/.*pid=\([0-9]*\).*/\1/p')
PID_WEB2=$(echo "$WEB2" | sed -n 's/.*pid=\([0-9]*\).*/\1/p')
if [ "$PID_WEB1" = "$PID_WEB2" ]; then
  echo "web pid should change after deploy"
  exit 1
fi

kill -TERM "$PID_MULTI"
wait "$PID_MULTI" 2>/dev/null || true
PID_MULTI=
rm -f "$CONTROL"

./portswap serve --listen 127.0.0.1:9100 \
  --health 'http://127.0.0.1:{port}/health' \
  --control "$CONTROL_SINGLE" \
  -- ./examples/demoapp/demoapp -version solo >"$LOG_SINGLE" 2>&1 &
PID_SINGLE=$!
for i in $(seq 1 40); do
  curl -sf --max-time 1 http://127.0.0.1:9100/health >/dev/null 2>&1 && break
  sleep 0.25
done
SOLO1=$(curl -sS http://127.0.0.1:9100/)
./portswap deploy --control "$CONTROL_SINGLE"
SOLO2=$(curl -sS http://127.0.0.1:9100/)
echo "single solo1=$SOLO1 solo2=$SOLO2"
case "$SOLO1" in *version=solo*) ;; *) exit 1;; esac
case "$SOLO2" in *version=solo*) ;; *) exit 1;; esac

# --- 초기 기동 실패 시 이미 띄운 서비스 정리 ---
LOG_INIT_FAIL=/tmp/portswap-smoke-initfail.log
CONTROL_BAD=/tmp/portswap-smoke-bad.sock
rm -f "$CONTROL_BAD"
set +e
./portswap serve --config examples/portswap-smoke-bad.yaml >"$LOG_INIT_FAIL" 2>&1
INIT_RC=$?
set -e
if [ "$INIT_RC" -eq 0 ]; then
  echo "expected non-zero exit for partial initial deploy"
  exit 1
fi
if pgrep -f 'demoapp.*smoke-good-marker' >/dev/null 2>&1; then
  echo "good service demoapp orphan still running after failed serve"
  pgrep -af 'demoapp.*smoke-good-marker' || true
  exit 1
fi
if curl -sf --max-time 1 http://127.0.0.1:9200/health >/dev/null 2>&1; then
  echo "port 9200 still serves after failed initial deploy"
  exit 1
fi

# --- 배포 헬스 실패 시 옛 PID 유지 + stderr 꼬리 ---
FAIL_MARKER=/tmp/portswap-smoke-fail-health
LOG_DEPLOY_FAIL=/tmp/portswap-smoke-deployfail.log
CONTROL_DEPLOY=/tmp/portswap-smoke-deploy.sock
rm -f "$FAIL_MARKER" "$CONTROL_DEPLOY"
./portswap serve --listen 127.0.0.1:9150 \
  --health 'http://127.0.0.1:{port}/health' \
  --control "$CONTROL_DEPLOY" \
  -- sh -c 'extra=""; [ -f '"$FAIL_MARKER"' ] && extra=-fail-health; exec ./examples/demoapp/demoapp -version=deploy-smoke $extra' \
  >"$LOG_DEPLOY_FAIL" 2>&1 &
PID_DEPLOY=$!
for i in $(seq 1 40); do
  curl -sf --max-time 1 http://127.0.0.1:9150/health >/dev/null 2>&1 && break
  sleep 0.25
done
DEPLOY_BODY1=$(curl -sS http://127.0.0.1:9150/)
case "$DEPLOY_BODY1" in *version=deploy-smoke*) ;; *) echo "unexpected body $DEPLOY_BODY1"; exit 1;; esac
PID_KEEP=$(echo "$DEPLOY_BODY1" | sed -n 's/.*pid=\([0-9]*\).*/\1/p')
touch "$FAIL_MARKER"
set +e
./portswap deploy --control "$CONTROL_DEPLOY" 2>/tmp/portswap-smoke-deploy-err.txt
DEPLOY_FAIL_RC=$?
set -e
if [ "$DEPLOY_FAIL_RC" -eq 0 ]; then
  echo "expected deploy to fail when health fails"
  kill -TERM "$PID_DEPLOY" 2>/dev/null || true
  exit 1
fi
if ! grep -q 'stderr' /tmp/portswap-smoke-deploy-err.txt && \
   ! grep -q 'listening' /tmp/portswap-smoke-deploy-err.txt; then
  echo "deploy error missing stderr tail:"
  cat /tmp/portswap-smoke-deploy-err.txt
  kill -TERM "$PID_DEPLOY" 2>/dev/null || true
  exit 1
fi
DEPLOY_BODY2=$(curl -sS http://127.0.0.1:9150/)
PID_AFTER=$(echo "$DEPLOY_BODY2" | sed -n 's/.*pid=\([0-9]*\).*/\1/p')
if [ "$PID_KEEP" != "$PID_AFTER" ]; then
  echo "pid changed after failed deploy ($PID_KEEP -> $PID_AFTER)"
  kill -TERM "$PID_DEPLOY" 2>/dev/null || true
  exit 1
fi
kill -TERM "$PID_DEPLOY"
wait "$PID_DEPLOY" 2>/dev/null || true
rm -f "$FAIL_MARKER" "$CONTROL_DEPLOY"

# --- 버전 출력 ---
PLAIN_VER=$(./portswap --version 2>/dev/null | tr -d '\n')
case "$PLAIN_VER" in *"portswap dev") ;; *) echo "plain build version: $PLAIN_VER"; exit 1;; esac
go build -ldflags "-X main.version=v0.2.0-smoke" -o portswap-versioned .
for flag in --version -v version; do
  VOUT=$(./portswap-versioned $flag 2>/dev/null | tr -d '\n')
  case "$VOUT" in *"portswap v0.2.0-smoke") ;; *) echo "flag $flag: $VOUT"; exit 1;; esac
done

echo "smoke OK"
