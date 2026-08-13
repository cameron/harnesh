#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
bash_bin="$(command -v bash)"
temporary="$(mktemp -d)"
session="harnesh-e2e-$$"
window=

capture() {
  tmux capture-pane -pt "$window" -S -1000 | tr -d '\r'
}

cleanup() {
  status=$?
  trap - EXIT INT TERM HUP
  if tmux has-session -t "$session" >/dev/null 2>&1; then
    tmux kill-session -t "$session"
  fi
  rm4agent -r "$temporary" >/dev/null 2>&1 || true
  exit "$status"
}
trap cleanup EXIT INT TERM HUP

wait_for() {
  pattern="$1"
  deadline=$((SECONDS + 15))
  while ((SECONDS < deadline)); do
    if capture | grep -Fq "$pattern"; then
      return 0
    fi
    sleep 0.25
  done
  echo "e2e: timed out waiting for: $pattern" >&2
  capture >&2
  return 1
}

wait_for_count() {
  pattern="$1"
  required="$2"
  deadline=$((SECONDS + 15))
  while ((SECONDS < deadline)); do
    if (( $(capture | grep -Fc "$pattern") >= required )); then
      return 0
    fi
    sleep 0.25
  done
  echo "e2e: timed out waiting for $required occurrences of: $pattern" >&2
  capture >&2
  return 1
}

mkdir -p "$temporary/home" "$temporary/prompts" "$temporary/state"
printf "PS1='harnesh-e2e$ '\n" > "$temporary/home/.bashrc"

fake_agent="$temporary/agent"
cat > "$fake_agent" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

count=0
if [[ -f "$FAKE_AGENT_COUNT" ]]; then
  read -r count < "$FAKE_AGENT_COUNT"
fi
count=$((count + 1))
printf '%s\n' "$count" > "$FAKE_AGENT_COUNT"
printf '%s\n' "$@" > "$FAKE_AGENT_LOG.$count"
cat > "$FAKE_AGENT_PROMPTS/$count"

case "$count" in
  1)
    printf '%s\n' '{"harness":"pi","session_id":"pi:harnesh-e2e-session","kind":"shell","answer":"","command":"cd \"$HARNESH_E2E_ROOT\" && export HARNESH_E2E_VALUE=shared && HARNESH_E2E_LOCAL=local && alias harnesh_alias=\u0027printf alias-ok\\n\u0027 && printf \u0027action-one:%s\\n\u0027 \"$PWD\""}'
    ;;
  2)
    printf '%s\n' '{"harness":"pi","session_id":"pi:harnesh-e2e-session","kind":"shell","answer":"","command":"harnesh_alias; printf \u0027value:%s:%s\\n\u0027 \"$HARNESH_E2E_VALUE\" \"$HARNESH_E2E_LOCAL\"; false"}'
    ;;
  *)
    printf '%s\n' '{"harness":"pi","session_id":"pi:harnesh-e2e-session","kind":"answer","answer":"fake agent complete","command":""}'
    ;;
esac
EOF
chmod 0755 "$fake_agent"

go build -o "$temporary/harnesh" .

export FAKE_AGENT_COUNT="$temporary/count"
export FAKE_AGENT_LOG="$temporary/args"
export FAKE_AGENT_PROMPTS="$temporary/prompts"
export HARNESH_E2E_ROOT="$repo_root"

window="$(tmux new-session -d -P -F '#{window_id}' -s "$session" -n harnesh \
  "cd '$temporary' && env HOME='$temporary/home' SHELL='$bash_bin' XDG_STATE_HOME='$temporary/state' HARNESH_AGENT_BIN='$fake_agent' HARNESH_E2E_ROOT='$repo_root' FAKE_AGENT_COUNT='$temporary/count' FAKE_AGENT_LOG='$temporary/args' FAKE_AGENT_PROMPTS='$temporary/prompts' '$temporary/harnesh'")"
tmux set-option -wt "$window" remain-on-exit off

wait_for 'harnesh-e2e$'
tmux send-keys -t "$window" 'please demonstrate shared state' Enter
wait_for "action-one:$repo_root"
wait_for 'alias-ok'
wait_for 'value:shared:local'
wait_for 'fake agent complete'

tmux send-keys -t "$window" 'history 20' Enter
sleep 0.25
if (( $(capture | grep -Fc 'harnesh_alias;') < 2 )); then
  echo 'e2e: agent action was not added to Bash history' >&2
  capture >&2
  exit 1
fi

tmux send-keys -t "$window" "printf 'direct-history\\n'" Enter
wait_for 'direct-history'
tmux send-keys -t "$window" ', summarize the direct command' Enter
wait_for 'harnesh-e2e$'
while [[ ! -f "$temporary/prompts/4" ]]; do sleep 0.1; done
grep -Fq "command: printf 'direct-history" "$temporary/prompts/4"
grep -Fq 'direct-history' "$temporary/prompts/4"

tmux send-keys -t "$window" ', confirm context is synchronized' Enter
while [[ ! -f "$temporary/prompts/5" ]]; do sleep 0.1; done
wait_for_count 'fake agent complete' 3
if grep -Fq "command: printf 'direct-history" "$temporary/prompts/5"; then
  echo 'e2e: direct shell event was delivered twice' >&2
  exit 1
fi

grep -Fxq -- --here "$temporary/args.1"
grep -Fxq -- --harnesh-turn "$temporary/args.1"
if grep -Eq '^(codex|pi|claude)$' "$temporary/args.1"; then
  echo 'e2e: Harnesh selected a harness instead of delegating selection to agent' >&2
  exit 1
fi
grep -Fxq -- --session "$temporary/args.2"
grep -Fxq pi:harnesh-e2e-session "$temporary/args.2"

session_id="$(find "$temporary/state/harnesh/sessions" -mindepth 1 -maxdepth 1 -type d -printf '%f\n')"
jq -e 'select(.command == "please demonstrate shared state" and .origin == "prompt")' \
  "$temporary/state/harnesh/sessions/$session_id/events.jsonl" >/dev/null
if grep -Fq 'command: please demonstrate shared state' "$temporary/prompts/4"; then
  echo 'e2e: agent prompt leaked into the direct shell context' >&2
  exit 1
fi
event_id="$(jq -r 'select(.origin == "user" and (.command | contains("direct-history"))) | .id' "$temporary/state/harnesh/sessions/$session_id/events.jsonl")"
history_output="$(XDG_STATE_HOME="$temporary/state" $temporary/harnesh history output "$event_id" --session "$session_id")"
[[ "$history_output" == *direct-history* ]]
XDG_STATE_HOME="$temporary/state" $temporary/harnesh sessions | grep -Fq "$session_id"

tmux send-keys -t "$window" exit Enter
deadline=$((SECONDS + 10))
while tmux has-session -t "$session" >/dev/null 2>&1 && ((SECONDS < deadline)); do
  sleep 0.1
done
if tmux has-session -t "$session" >/dev/null 2>&1; then
  echo 'e2e: initial Harnesh session did not exit' >&2
  exit 1
fi

window="$(tmux new-session -d -P -F '#{window_id}' -s "$session" -n resume \
  "cd '$temporary' && env HOME='$temporary/home' SHELL='$bash_bin' XDG_STATE_HOME='$temporary/state' HARNESH_AGENT_BIN='$fake_agent' '$temporary/harnesh' resume '$session_id'")"
tmux set-option -wt "$window" remain-on-exit off
wait_for 'harnesh-e2e$'
if capture | grep -Fq 'action-one:'; then
  echo 'e2e: resume replayed a previous command' >&2
  exit 1
fi
tmux send-keys -t "$window" pwd Enter
wait_for "$repo_root"
tmux send-keys -t "$window" 'if [[ -z ${HARNESH_E2E_VALUE+x} ]]; then printf '\''resume-fresh-shell\n'\''; fi' Enter
wait_for 'resume-fresh-shell'

echo 'e2e: ok'
