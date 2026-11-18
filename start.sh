#!/bin/sh
# ARTEX supervisor startup script (Linux / macOS / Docker ENTRYPOINT)
#
# Usage:
#   ./start.sh                       run in the foreground (Ctrl-C to stop)
#   nohup ./start.sh >artex.log 2>&1 &   run detached in the background
#   ./start.sh -addr :9000           extra args are passed straight through to artex
#
# It does exactly one thing: run artex, and after the process exits, decide
# whether to bring it back up based on its exit code.
#
#   0      user stopped it normally   -> exit the loop
#   75     the program requested a restart -> rerun immediately (the page's "update" or "rollback" was clicked)
#   other  crash                      -> back off and rerun (1->2->4... up to 60 seconds)
#
# Deliberately no download, SHA256 verification, or swap logic here: that
# would need to be written twice, once for sh and once for bat, and it's
# exactly the part that must not go wrong -- if a binary that can't run gets
# swapped in, this script would faithfully keep relaunching it forever, and
# the user would have to fix it by hand on the machine. So all verification/
# swapping stays in Go (the selfupdate package), done by artex itself on
# startup, keeping this script dumb on purpose.
set -u

cd "$(dirname "$0")" || exit 1

BIN=./artex
[ -x "$BIN" ] || { echo "[artex] executable not found: $BIN" >&2; exit 1; }

RESTART_CODE=75
MAX_DELAY=60

child=0
stopping=0

# Forward the stop signal to the artex process itself.
#
# This is required under Docker: docker stop only sends SIGTERM to PID 1 (this
# script), not to child processes. Without forwarding it, artex never gets the
# signal, can't shut down gracefully, and gets hard-killed by SIGKILL after 10
# seconds, cutting off any running task mid-way.
forward() {
	stopping=1
	if [ "$child" -ne 0 ]; then
		kill -TERM "$child" 2>/dev/null || true
	fi
}
trap forward INT TERM

# Wait for either the application or its restart delay. A stop that arrived
# between starting the child and recording its PID must reach that child too.
wait_for_child() {
	if [ "$stopping" -eq 1 ]; then
		kill -TERM "$child" 2>/dev/null || true
	fi
	wait "$child"
	code=$?
	if [ "$code" -gt 128 ]; then
		wait "$child"
		code=$?
	fi
	child=0
}

delay=1
while [ "$stopping" -eq 0 ]; do
	"$BIN" "$@" &
	child=$!

	# A signal interrupts wait and makes it return >128. At that point the child
	# is actually still doing a graceful shutdown, and wait must be called again
	# to get its real exit code.
	wait_for_child

	if [ "$stopping" -eq 1 ]; then
		echo "[artex] stopped"
		exit 0
	fi

	case "$code" in
		0)
			echo "[artex] exited normally"
			exit 0
			;;
		"$RESTART_CODE")
			# Update/rollback is ready: artex finishes the swap on startup after this rerun (see selfupdate.Bootstrap).
			echo "[artex] restart requested (applying new version)..."
			delay=1
			;;
		*)
			echo "[artex] exited abnormally (code=$code), restarting in ${delay}s" >&2
			sleep "$delay" &
			child=$!
			wait_for_child
			delay=$((delay * 2))
			[ "$delay" -gt "$MAX_DELAY" ] && delay=$MAX_DELAY
			;;
	esac
done
echo "[artex] stopped"
exit 0
