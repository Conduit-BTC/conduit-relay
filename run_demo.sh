#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$SCRIPT_DIR"
RELAY_URL="ws://127.0.0.1:3334"
LOG_FILE="$ROOT_DIR/tmp/scope2-demo.log"
PORT=""

relay_pid=""

cleanup() {
  if [[ -n "$relay_pid" ]] && kill -0 "$relay_pid" >/dev/null 2>&1; then
    kill "$relay_pid" >/dev/null 2>&1 || true
    wait "$relay_pid" >/dev/null 2>&1 || true
  fi
}

fail() {
  echo "[FAIL] $1" >&2
  if [[ -f "$LOG_FILE" ]]; then
    echo "--- relay log ---" >&2
    tail -n 60 "$LOG_FILE" >&2 || true
  fi
  exit 1
}

trap cleanup EXIT

mkdir -p "$ROOT_DIR/tmp"

for p in 3334 3335 3336 3337 3338 3339; do
  if ! ss -ltn "( sport = :$p )" | grep -q ":$p"; then
    PORT="$p"
    break
  fi
done

if [[ -z "${PORT:-}" ]]; then
  fail "could not find a free local port"
fi

RELAY_URL="ws://127.0.0.1:$PORT"
HTTP_URL="http://127.0.0.1:$PORT"

echo "[1/6] Starting Scope2 demo relay..."
(
  cd "$ROOT_DIR"
  PORT="$PORT" go run ./cmd/demo >"$LOG_FILE" 2>&1
) &
relay_pid="$!"

echo "[2/6] Waiting for relay startup..."
for _ in $(seq 1 80); do
  if curl -fsS -H 'Accept: application/nostr+json' "$HTTP_URL" >/dev/null 2>&1; then
    break
  fi
  sleep 0.25
done

curl -fsS -H 'Accept: application/nostr+json' "$HTTP_URL" >/dev/null 2>&1 || fail "relay did not start"

relay_info_raw="$(curl -sS -H 'Accept: application/nostr+json' "$HTTP_URL")"
grep -q '"conduit_l2"' <<<"$relay_info_raw" || fail "NIP-11 does not expose conduit_l2 extension"

sec="$(nak key generate | tr -d '\r\n')"
[[ -n "$sec" ]] || fail "failed to generate signing key"

echo "[3/6] Publishing demo products..."
nak event --sec "$sec" -k 30402 -d apple -c '{"title":"Apple","summary":"Red fruit","price":19,"currency":"USD","updatedAt":1000}' "$RELAY_URL" >/dev/null
nak event --sec "$sec" -k 30402 -d banana -c '{"title":"Banana","summary":"Yellow fruit","price":7,"currency":"USD","updatedAt":1001}' "$RELAY_URL" >/dev/null
nak event --sec "$sec" -k 30402 -d carrot -c '{"title":"Carrot","summary":"Orange root","price":12,"currency":"USD","updatedAt":1002}' "$RELAY_URL" >/dev/null

echo "[4/6] Verifying deterministic price sort..."
sorted_output="$(nak req -k 30402 -l 10 --search 'conduit-l2:q=;sort=price_asc' "$RELAY_URL")"
sorted_prices="$(printf '%s\n' "$sorted_output" | python3 -c '
import json,sys
prices=[]
for line in sys.stdin:
    line=line.strip()
    if not line:
        continue
    try:
        obj=json.loads(line)
    except Exception:
        continue
    event=None
    if isinstance(obj,list) and len(obj)>=3 and obj[0]=="EVENT":
        event=obj[2]
    elif isinstance(obj,dict) and "kind" in obj:
        event=obj
    if not isinstance(event,dict) or event.get("kind")!=30402:
        continue
    try:
        content=json.loads(event.get("content","{}"))
    except Exception:
        continue
    if "price" in content:
        prices.append(str(int(content["price"])))
print(" ".join(prices))
')"
[[ "$sorted_prices" == "7 12 19" ]] || fail "unexpected price order: '$sorted_prices'"

echo "[5/6] Verifying text query behavior..."
apple_output="$(nak req -k 30402 -l 10 --search 'conduit-l2:q=apple;sort=newest' "$RELAY_URL")"
apple_titles="$(printf '%s\n' "$apple_output" | python3 -c '
import json,sys
titles=[]
for line in sys.stdin:
    line=line.strip()
    if not line:
        continue
    try:
        obj=json.loads(line)
    except Exception:
        continue
    event=None
    if isinstance(obj,list) and len(obj)>=3 and obj[0]=="EVENT":
        event=obj[2]
    elif isinstance(obj,dict) and "kind" in obj:
        event=obj
    if not isinstance(event,dict) or event.get("kind")!=30402:
        continue
    try:
        content=json.loads(event.get("content","{}"))
    except Exception:
        continue
    title=content.get("title")
    if title:
        titles.append(title)
print("|".join(titles))
')"
[[ "$apple_titles" == "Apple" ]] || fail "unexpected text-query titles: '$apple_titles'"

echo "[6/6] Verifying protected kind gating (NIP-42 required)..."
protected_output="$(nak req -k 1059 "$RELAY_URL" 2>&1 || true)"
grep -q 'auth-required' <<<"$protected_output" || fail "kind 1059 query was not auth-gated"

echo "[OK] Scope2 demo script finished successfully."
