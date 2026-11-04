#!/usr/bin/env bash
# Smoke test: build the Aether binary, start it against a throwaway config, and
# probe /healthz. The server and temporary tree are always cleaned up.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

tmp="$(mktemp -d "${TMPDIR:-/tmp}/aether-smoke.XXXXXX")"
pid=""
cleanup() {
  if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  fi
  rm -rf "$tmp"
}
trap cleanup EXIT

port="${AETHER_SMOKE_PORT:-$((18000 + RANDOM % 1000))}"
config="$tmp/config"
mkdir -p "$config"

cat >"$config/system.json" <<JSON
{
  "server": { "port": $port, "db_path": "aether.db" },
  "persona": { "name": "Aether", "prefixes": ["!", "/", "Aether "] },
  "auth": { "jwt_secret": "smoke-only-secret" },
  "limits": { "rate_per_second": 1, "burst": 5, "queue_depth": 16 },
  "default_provider": "signal",
  "providers": {
    "signal": {
      "type": "msg",
      "account": "+15555550100",
      "base_url": "http://127.0.0.1:1",
      "db_path": "$tmp/signal-cache.db"
    }
  }
}
JSON

go build -o "$tmp/aether" ./cmd/aether

"$tmp/aether" serve --config-dir "$config" --addr "127.0.0.1:$port" >"$tmp/aether.log" 2>&1 &
pid=$!

url="http://127.0.0.1:$port/healthz"
for _ in $(seq 1 100); do
  if ! kill -0 "$pid" 2>/dev/null; then
    echo "aether exited before becoming healthy:" >&2
    cat "$tmp/aether.log" >&2
    exit 1
  fi

  code="$(curl -sS -o "$tmp/body" -w '%{http_code}' --max-time 1 "$url" 2>/dev/null || true)"
  if [[ "$code" == "200" ]]; then
    body="$(cat "$tmp/body")"
    if [[ "$body" != "ok" ]]; then
      echo "unexpected /healthz body: $body" >&2
      exit 1
    fi
    echo "healthz: $body"
    exit 0
  fi

  sleep 0.1
done

echo "aether did not become healthy at $url" >&2
cat "$tmp/aether.log" >&2
exit 1
