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
  rm -f "$CONTROL" "$CONTROL_SINGLE" /tmp/portswap-smoke-fail-health
  fuser -k 9202/tcp 2>/dev/null || true
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

# --- version (dev vs ldflags) ---
go build -o portswap-dev .
OUT=$(./portswap-dev --version); case "$OUT" in "portswap dev") ;; *) echo "bad --version: $OUT"; exit 1;; esac
OUT=$(./portswap-dev -v); case "$OUT" in "portswap dev") ;; *) echo "bad -v: $OUT"; exit 1;; esac
OUT=$(./portswap-dev version); case "$OUT" in "portswap dev") ;; *) echo "bad version: $OUT"; exit 1;; esac
go build -ldflags '-X main.version=v0.2.0' -o portswap-release .
OUT=$(./portswap-release --version); case "$OUT" in "portswap v0.2.0") ;; *) echo "bad release --version: $OUT"; exit 1;; esac
mv ./portswap-release ./portswap

# --- initial deploy failure: ok service must not leave orphans ---
LOG_FAIL=/tmp/portswap-smoke-fail-init.log
rm -f /tmp/portswap-fail-init.sock
set +e
./portswap serve --config examples/portswap-fail-init.yaml >"$LOG_FAIL" 2>&1
FAIL_RC=$?
set -e
if [ "$FAIL_RC" -eq 0 ]; then
  echo "expected non-zero exit on initial deploy failure"
  cat "$LOG_FAIL"
  exit 1
fi
if pgrep -f 'smoke-ok-marker' >/dev/null 2>&1; then
  echo "orphan demoapp still running after failed initial deploy"
  pgrep -af smoke-ok-marker
  exit 1
fi
if curl -sf --max-time 0.5 http://127.0.0.1:9200/health >/dev/null 2>&1; then
  echo "port 9200 still serves after failed initial deploy"
  exit 1
fi

# --- deploy health failure keeps old process and error includes stderr tail ---
CONTROL_FAIL=/tmp/portswap-smoke-deploy-fail.sock
LOG_DEPLOY_FAIL=/tmp/portswap-smoke-deploy-fail.log
FAIL_HEALTH_FLAG=/tmp/portswap-smoke-fail-health
rm -f "$CONTROL_FAIL" "$FAIL_HEALTH_FLAG"
./portswap serve --listen 127.0.0.1:9202 \
  --health 'http://127.0.0.1:{port}/health' \
  --health-timeout 10s \
  --control "$CONTROL_FAIL" \
  -- sh -c 'if [ -f /tmp/portswap-smoke-fail-health ]; then extra=-fail-health; rm -f /tmp/portswap-smoke-fail-health; fi; exec ./examples/demoapp/demoapp -version deployfail-ok $extra' \
  >"$LOG_DEPLOY_FAIL" 2>&1 &
PID_DF=$!
for i in $(seq 1 40); do
  curl -sf --max-time 1 http://127.0.0.1:9202/health >/dev/null 2>&1 && break
  sleep 0.25
done
BODY_DF1=$(curl -sS http://127.0.0.1:9202/)
set +e
touch "$FAIL_HEALTH_FLAG"
DEPLOY_ERR=$(./portswap deploy --control "$CONTROL_FAIL" 2>&1)
DEPLOY_RC=$?
set -e
if [ "$DEPLOY_RC" -eq 0 ]; then
  echo "deploy should fail on -fail-health"
  exit 1
fi
case "$DEPLOY_ERR" in *demoapp*) ;; *) echo "deploy error missing stderr tail: $DEPLOY_ERR"; exit 1;; esac
BODY_DF2=$(curl -sS http://127.0.0.1:9202/)
case "$BODY_DF1" in *deployfail-ok*) ;; *) exit 1;; esac
case "$BODY_DF2" in *deployfail-ok*) ;; *) exit 1;; esac
PID1=$(echo "$BODY_DF1" | sed -n 's/.*pid=\([0-9]*\).*/\1/p')
PID2=$(echo "$BODY_DF2" | sed -n 's/.*pid=\([0-9]*\).*/\1/p')
if [ "$PID1" != "$PID2" ]; then
  echo "pid should stay same after failed deploy ($PID1 vs $PID2)"
  exit 1
fi
kill -TERM "$PID_DF" 2>/dev/null || true
wait "$PID_DF" 2>/dev/null || true
rm -f "$CONTROL_FAIL"

echo "smoke OK"
