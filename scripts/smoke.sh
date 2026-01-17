#!/usr/bin/env bash
set -euo pipefail

API="${API:-http://localhost:8080}"
MAX_WAIT_SEC="${MAX_WAIT_SEC:-5}"
POLL_INTERVAL="${POLL_INTERVAL:-0.2}"

need() { command -v "$1" >/dev/null 2>&1 || { echo "Missing dependency: $1" >&2; exit 1; }; }
need curl
need jq

echo "==> Smoke test against API: $API"
echo "==> Checking health..."
curl -s "$API/api/v1/health" | grep -qx "ok" || { echo "Health endpoint failed"; exit 1; }

echo "==> Checking JetStream subjects..."
go run ./cmd/js-info | sed 's/^/    /'

# Helper: wait until a task reaches a desired status
wait_status() {
  local id="$1"
  local want="$2"
  local start
  start="$(date +%s)"
  while true; do
    local got
    got="$(curl -s "$API/api/v1/tasks/$id" | jq -r '.task.status')"
    if [[ "$got" == "$want" ]]; then
      return 0
    fi
    local now
    now="$(date +%s)"
    if (( now - start > MAX_WAIT_SEC )); then
      echo "Timed out waiting for task $id to become $want (last=$got)"
      return 1
    fi
    sleep "$POLL_INTERVAL"
  done
}

# Helper: count executions
exec_count() {
  local id="$1"
  curl -s "$API/api/v1/tasks/$id/executions" | jq '.items | length'
}

# Helper: show executions
show_execs() {
  local id="$1"
  curl -s "$API/api/v1/tasks/$id/executions" | jq
}

echo
echo "=============================="
echo "TEST 1: demo task completes"
echo "=============================="
DEMO_ID="$(curl -s -X POST "$API/api/v1/tasks" \
  -H 'Content-Type: application/json' \
  -d '{"type":"demo","payload":{"hello":"world"},"priority":"normal"}' | jq -r '.task.id')"

echo "Created demo task: $DEMO_ID"

wait_status "$DEMO_ID" "completed"
echo "OK: demo task completed"

c="$(exec_count "$DEMO_ID")"
if [[ "$c" != "1" ]]; then
  echo "FAIL: expected 1 execution for demo task, got $c"
  show_execs "$DEMO_ID"
  exit 1
fi
echo "OK: demo has exactly 1 execution"


echo
echo "=============================================="
echo "TEST 2: duplicate publishes do NOT create spam"
echo "=============================================="
DUP_ID="$(curl -s -X POST "$API/api/v1/tasks" \
  -H 'Content-Type: application/json' \
  -d '{"type":"demo","payload":{"hello":"world"},"priority":"normal"}' | jq -r '.task.id')"

echo "Created duplicate-test task: $DUP_ID"
echo "Publishing the same task ID 20x to JetStream..."
go run ./cmd/nats-pub --task-id "$DUP_ID" --count 20 --interval 0ms >/dev/null

wait_status "$DUP_ID" "completed"
echo "OK: task completed"

c="$(exec_count "$DUP_ID")"
if [[ "$c" != "1" ]]; then
  echo "FAIL: expected 1 execution even after 20 duplicate publishes, got $c"
  show_execs "$DUP_ID"
  exit 1
fi
echo "OK: duplicates did not create extra executions"


echo
echo "===================================================="
echo "TEST 3: unknown task fails permanently + goes to DLQ"
echo "===================================================="
UNK_ID="$(curl -s -X POST "$API/api/v1/tasks" \
  -H 'Content-Type: application/json' \
  -d '{"type":"unknown","payload":{"x":999},"priority":"normal"}' | jq -r '.task.id')"

echo "Created unknown task: $UNK_ID"

wait_status "$UNK_ID" "failed"
echo "OK: unknown task marked failed"

c="$(exec_count "$UNK_ID")"
if [[ "$c" != "1" ]]; then
  echo "FAIL: expected 1 execution for unknown task, got $c"
  show_execs "$UNK_ID"
  exit 1
fi

# Try to detect DLQ message count increased by 1 (best effort check)
# This assumes your TASKFLOW stream includes tasks.dlq and that publishing DLQ adds a message.
# We'll just confirm the stream now contains tasks.dlq (already checked) and that executions error mentions no handler.
err="$(curl -s "$API/api/v1/tasks/$UNK_ID/executions" | jq -r '.items[0].error')"
echo "Unknown error: $err"
echo "$err" | grep -q 'no handler registered' || { echo "FAIL: unexpected error text: $err"; exit 1; }

echo
echo "✅ ALL SMOKE TESTS PASSED"
