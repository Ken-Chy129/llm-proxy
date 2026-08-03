#!/bin/sh
# Pull the latest code and rebuild/restart the container, in place, on the server.
# Usage (on the server):  cd ~/apps/llm-proxy && ./deploy.sh
#
# The whole body lives in main() so the shell parses it fully before running —
# that way the `git pull` below can overwrite this very file mid-run without
# corrupting execution.
set -e

main() {
	cd "$(dirname "$0")"

	echo "=== git pull ==="
	git pull --ff-only

	echo "=== build + restart container ==="
	docker compose up -d --build

	echo "=== prune dangling images ==="
	docker image prune -f >/dev/null 2>&1 || true

	echo "=== status ==="
	docker compose ps

	# Poll rather than sleep-then-probe. Startup fetches upstream quotas before it
	# binds the port (~4s for three Claude accounts), so a fixed `sleep 3` reported
	# "Empty reply from server" on a deploy that was in fact fine — a health check
	# that cries wolf is worse than none, because the next real failure reads the
	# same. 20 tries x 2s covers a cold start with room to spare.
	echo "=== health ==="
	i=0
	while [ "$i" -lt 20 ]; do
		if curl -fsS --max-time 3 http://127.0.0.1:9090/health; then
			echo
			echo "healthy after $((i * 2))s"
			return 0
		fi
		i=$((i + 1))
		sleep 2
	done

	echo "UNHEALTHY: /health did not answer within 40s" >&2
	docker compose logs --tail 40 llm-proxy >&2
	return 1
}

main "$@"
