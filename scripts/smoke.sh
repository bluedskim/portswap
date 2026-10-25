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

echo "smoke OK"
