#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SESSION="${ATY_TMUX_SESSION:-aty-smoke-$$}"
WINDOW="${ATY_TMUX_WINDOW:-aty}"
WINDOW_TARGET=""
TIMEOUT_SECONDS="${ATY_TMUX_TIMEOUT:-240}"
PROMPT="${ATY_TMUX_PROMPT:-please run ls in the shell}"
EXPECTED_COMMAND="${ATY_TMUX_EXPECTED_COMMAND:-ls}"
EXPECTED_LINE="${ATY_TMUX_EXPECTED_LINE:-go.mod  go.sum  main.go  main_test.go  PI_PROTOTYPE.md  SPEC.md  test}"
CLEANED_UP=0

capture() {
	tmux capture-pane -pt "$WINDOW_TARGET" -S -500 | tr -d '\r'
}

prompt_ready() {
	capture | grep -Eq '(^|[[:space:]])[$#>]$'
}

cleanup() {
	status=$?
	trap - EXIT INT TERM HUP
	if (( CLEANED_UP )); then
		exit "$status"
	fi
	CLEANED_UP=1
	if [[ "${ATY_KEEP_TMUX:-}" == "1" ]]; then
		echo "tmux session left running: $SESSION" >&2
		exit "$status"
	fi
	if tmux has-session -t "$SESSION" 2>/dev/null; then
		tmux kill-session -t "$SESSION"
	fi
	exit "$status"
}
trap cleanup EXIT INT TERM HUP

if ! command -v tmux >/dev/null 2>&1; then
	echo "e2e: tmux is required" >&2
	exit 127
fi

if tmux has-session -t "$SESSION" 2>/dev/null; then
	echo "e2e: session already exists: $SESSION" >&2
	exit 2
fi

WINDOW_TARGET="$(tmux new-session -d -P -F '#{window_id}' -s "$SESSION" -n "$WINDOW" "cd '$ROOT' && go run .")"

deadline=$((SECONDS + TIMEOUT_SECONDS))
while (( SECONDS < deadline )); do
	if prompt_ready; then
		break
	fi
	sleep 1
done
if ! prompt_ready; then
	echo "e2e: timed out waiting for aty shell prompt" >&2
	capture >&2
	exit 1
fi

tmux send-keys -t "$WINDOW_TARGET" "$PROMPT" Enter

while (( SECONDS < deadline )); do
	output="$(capture)"
	if printf '%s\n' "$output" | grep -Fq "> $EXPECTED_COMMAND" &&
		printf '%s\n' "$output" | grep -Fxq "$EXPECTED_LINE"; then
		echo "e2e: ok"
		exit 0
	fi
	if printf '%s\n' "$output" | grep -q 'aty:'; then
		echo "e2e: aty reported an error before expected output" >&2
		printf '%s\n' "$output" >&2
		exit 1
	fi
	sleep 2
done

echo "e2e: timed out waiting for exact line: $EXPECTED_LINE" >&2
echo "e2e: also expected visible shell prompt plus command: $EXPECTED_COMMAND" >&2
echo "e2e: captured pane:" >&2
capture >&2
exit 1
