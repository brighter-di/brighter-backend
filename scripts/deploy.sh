#!/usr/bin/env bash
# Deploy brighterd to the Brighter VPS (`ssh onepage`).
#
#   ./scripts/deploy.sh            build, upload, restart, wait for /health/ready
#
# One-time setup on the box (not done by this script):
#   - Postgres role + database, and /etc/brighter-backend/env (mode 600) with
#     DATABASE_URL, BACKTEST_ENGINE_API_KEY_ID/SECRET, PORTFOLIO_ENGINE_API_KEY, CHAINS,
#     CORS_ORIGINS. See .env.example.
#   - deploy/brighter-backend.service → /etc/systemd/system/, then
#     `systemctl daemon-reload && systemctl enable brighter-backend`.
#   - an nginx location proxying to 127.0.0.1:3200.
#
# Secrets are never uploaded from here: the env file lives on the server only.
set -euo pipefail

HOST="${DEPLOY_HOST:-onepage}"
DIR=/opt/brighter-backend
ARCH="${GOARCH:-amd64}"

cd "$(dirname "$0")/.."

echo "→ test"
go vet ./...
go test ./...

echo "→ build linux/$ARCH"
CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath -ldflags="-s -w" -o bin/brighterd-linux ./cmd/brighterd

echo "→ upload"
ssh "$HOST" "mkdir -p $DIR"
scp bin/brighterd-linux "$HOST:$DIR/brighterd.new"

echo "→ migrate + restart"
ssh "$HOST" "set -e
  chmod 755 $DIR/brighterd.new
  mv $DIR/brighterd.new $DIR/brighterd
  set -a; . /etc/brighter-backend/env; set +a
  $DIR/brighterd migrate
  systemctl restart brighter-backend"

echo "→ waiting for /health/ready"
for _ in $(seq 1 30); do
  if ssh "$HOST" "curl -fsS http://127.0.0.1:3200/health/ready" 2>/dev/null; then
    echo
    ssh "$HOST" "journalctl -u brighter-backend -n 5 --no-pager"
    echo "✓ deployed"
    exit 0
  fi
  sleep 1
done
echo "✗ brighter-backend did not become ready in 30s" >&2
ssh "$HOST" "journalctl -u brighter-backend -n 40 --no-pager" >&2
exit 1
