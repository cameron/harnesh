# Harnesh

Harnesh is a persistent interactive shell that dispatches prose prompts through `agent`. The configured Codex, Pi, or Claude harness and the user can run commands in the same PTY-backed Bash or Fish process. Shared-shell actions therefore see and change the live working directory, variables, aliases, functions, jobs, history, and terminal output.

The selected harness can still use its built-in shell tools for isolated repository work and background processing. Its structured response tells Harnesh whether to show an answer or run one command in the visible shared shell. Harnesh returns that command's status and output to the same native harness session. The loop has a default limit of 32 shared-shell actions for one user prompt.

## Requirements

- `agent` with `--harnesh-turn` support
- Codex, Pi, or Claude installed and configured for non-interactive use
- Bash or Fish for structured command history and output
- Linux PTY support

Set `HARNESH_AGENT_BIN` to another executable for testing or for a local `agent` wrapper.

## Use

Start a new session:

```console
$ harnesh
harnesh: session 20260812-120000-0123456789abcdef
```

Enter shell commands normally. Prefix a prompt with a comma to route it to the configured agent:

```console
$ cd ~/src/example
$ export RELEASE_CHANNEL=staging
$ , inspect the current project and show me the next useful check
```

Input with an unknown command head also uses the prompt path. This preserves the earlier command-not-found behavior. Harnesh does not yet use a general command-versus-prose classifier, so use the comma form when the first word is also a real command.

When the agent requests a shared-shell action, Harnesh prints and runs it in the active shell. Other input is locked until the action loop ends. Press Ctrl-C to cancel the agent turn and restore shell input.

## Sessions and history

Each normal start creates a new Harnesh session. Resume a durable session by ID or choose the most recently updated session:

```console
$ harnesh sessions
$ harnesh resume 20260812-120000-0123456789abcdef
$ harnesh resume --last
```

Resume creates a fresh shell in the last recorded working directory and continues the recorded native harness session. It does not replay commands, variables, aliases, functions, or jobs. If the recorded directory no longer exists, Harnesh warns and uses the caller's current directory. If the selected harness no longer has the native session, the next agent turn stops with an adapter error instead of silently starting unrelated context.

Inspect structured events or recover the exact output for one event:

```console
$ harnesh history tail 20 --session SESSION_ID
$ harnesh history output EVENT_ID --session SESSION_ID
```

Inside a Harnesh shell, `--session` is optional. The command uses `HARNESH_SESSION_ID`. Outside a session, it uses the most recently updated session when no ID is given.

## Durable context

Session state is private under `${XDG_STATE_HOME:-$HOME/.local/state}/harnesh/sessions/`. Direct user shell events record a sequence number, stable event ID, command, initial and final directories, timestamps, exit status, output size, and an output reference. Exact output is stored in mode `0600` blobs. Session directories use mode `0700`.

Before each successful agent turn, Harnesh sends only direct shell events that the adapter has not seen. Prompt events and agent-requested actions do not re-enter this synchronization batch. Large outputs use a 32 KiB head and 32 KiB tail projection, with an exact `harnesh history output` reference. One synchronization batch is limited to 256 KiB and has a stable batch ID, so a failed turn can retry the same context without advancing its cursor.

The native harness remains the authority for conversational context and compaction. Harnesh stores an opaque harness-prefixed session ID and the shell-event cursor. It does not copy or reinterpret the harness transcript.

Unsupported shells still run through the PTY, but Harnesh can only keep a bounded raw transcript for them. Structured command boundaries, shared-shell agent actions, prompt hooks, and exact event history currently require Bash or Fish.

## Configuration

- `HARNESH_AGENT_BIN`: executable used in place of `agent`
- `HARNESH_ACTION_LIMIT`: positive shared-shell action limit; default `32`
- `XDG_STATE_HOME`: root for durable Harnesh session state
- `SHELL`: interactive shell executable; Harnesh falls back to `bash` when unset

For a new Harnesh session, normal `agent` configuration selects the harness. For example:

```console
$ AGENT_BIN=pi harnesh
$ AGENT_BIN=codex harnesh
```

Harnesh itself starts a turn with:

```console
agent --here --harnesh-turn
```

`agent` translates this common protocol to the selected harness's native structured-output and resume options. It returns an opaque ID such as `pi:NATIVE_ID`. Later turns pass the complete ID with `--session`; its prefix keeps that Harnesh session on Pi even if `AGENT_BIN` changes. Existing Codex-only Harnesh sessions migrate to `codex:NATIVE_ID` when they open.

Pi continues to use its normal model and provider configuration. This includes local Ollama-backed entries in Pi's `models.json` and selection through `AGENT_PI_FLAGS`; Harnesh does not replace or remove that configuration.

## Development

Run the complete gate:

```console
$ make test
```

The gate runs Go tests, a deterministic fake-`agent` tmux test, and `nix flake check`. The end-to-end test covers native thread resume, multiple shared-shell actions, failure status, working-directory and variable state, aliases, history, direct-event synchronization, exact output retrieval, and restart behavior.
