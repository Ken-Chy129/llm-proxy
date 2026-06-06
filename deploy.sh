#!/bin/bash
# Deploy cli-proxy to remote server
# Usage: ./deploy.sh [server_ip] [password]

SERVER=${1:-YOUR_SERVER_IP}
PASSWORD=${2:-'REDACTED'}

set -e

echo "=== Building for Linux ==="
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o cli-proxy-linux .
echo "Built: $(du -h cli-proxy-linux | cut -f1)"

echo "=== Stopping remote service ==="
export SSHPASS="$PASSWORD"
sshpass -e ssh -o StrictHostKeyChecking=no root@$SERVER "pkill -9 -f cli-proxy; sleep 1; rm -f ~/cli-proxy/cli-proxy" 2>/dev/null || true

echo "=== Uploading binary ==="
sshpass -e scp -o StrictHostKeyChecking=no cli-proxy-linux root@$SERVER:~/cli-proxy/cli-proxy

echo "=== Starting service ==="
sshpass -e ssh -o StrictHostKeyChecking=no root@$SERVER "chmod +x ~/cli-proxy/cli-proxy; cd ~/cli-proxy && nohup ./cli-proxy -config config.yaml > /var/log/cli-proxy.log 2>&1 &"

echo "=== Waiting for startup ==="
sleep 15

if curl -s --max-time 10 "https://ken-chy129.cn/health" | grep -q '"ok"'; then
    echo "=== Deploy success ==="
else
    echo "=== Health check failed, checking logs ==="
    sshpass -e ssh -o StrictHostKeyChecking=no root@$SERVER "tail -5 /var/log/cli-proxy.log" 2>/dev/null || true
fi

rm -f cli-proxy-linux
