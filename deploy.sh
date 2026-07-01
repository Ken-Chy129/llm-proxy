#!/bin/sh
# Pull the latest code and rebuild/restart the container, in place, on the server.
# Usage (on the server):  cd ~/apps/llm-proxy && ./redeploy.sh
set -e
cd "$(dirname "$0")"

echo "=== git pull ==="
git pull --ff-only

echo "=== build + restart container ==="
docker compose up -d --build

echo "=== prune dangling images ==="
docker image prune -f >/dev/null 2>&1 || true

echo "=== status ==="
docker compose ps
echo "=== health ==="
sleep 3
curl -fsS --max-time 8 http://127.0.0.1:9090/health && echo
