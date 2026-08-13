# Harnesh

Harnesh is a persistent interactive shell that dispatches prose prompts to Codex through `agent`. The user and Codex can run commands in the same PTY-backed Bash or Fish process. Shared-shell actions therefore see and change the live working directory, variables, aliases, functions, jobs, history, and terminal output.

Codex can still use its built-in shell tools for isolated repository work and background processing. Its structured response tells Harnesh whether to show an answer or run one command in the visible shared shell. Harnesh returns that command's status and output to the same native Codex thread. The loop has a default limit of 32 shared-shell actions for one user prompt.

## Requirements

- `agent` with the Codex selector available
- Codex authentication configured for `codex exec`
- Bash or Fish for structured command history and output
- Linux PTY support

Set `HARNESH_AGENT_BIN` to another executable for testing or for a local `agent` wrapper.

## Use

Start a new session:

```console
$ harnesh
harnesh: session 20260812-120000-0123456789abcdef
```

Enter shell commands normally. Prefix a prompt with a comma to route it to Codex:

```console
$ cd ~/src/example
$ export RELEASE_CHANNEL=staging
$ , inspect the current project and show me the next useful check
```

Input with an unknown command head also uses the prompt path. This preserves the earlier command-not-found behavior. Harnesh does not yet use a general command-versus-prose classifier, so use the comma form when the first word is also a real command.

When Codex requests a shared-shell action, Harnesh prints and runs it in the active shell. Other input is locked until the action loop ends. Press Ctrl-C to cancel the Codex turn and restore shell input.

## Sessions and history

Each normal start creates a new Harnesh session. Resume a durable session by ID or choose the most recently updated session:

```console
$ harnesh sessions
$ harnesh resume 20260812-120000-0123456789abcdef
$ harnesh resume --last
```

Resume creates a fresh shell in the last recorded working directory and continues the recorded native Codex thread. It does not replay commands, variables, aliases, functions, or jobs. If the recorded directory no longer exists, Harnesh warns and uses the caller's current directory. If Codex no longer has the native thread, the next agent turn stops with an adapter error instead of silently starting unrelated context.

Inspect structured events or recover the exact output for one event:

```console
$ harnesh history tail 20 --session SESSION_ID
$ harnesh history output EVENT_ID --session SESSION_ID
```

Inside a Harnesh shell, `--session` is optional. The command uses `HARNESH_SESSION_ID`. Outside a session, it uses the most recently updated session when no ID is given.

## Durable context

Session state is private under `${XDG_STATE_HOME:-$HOME/.local/state}/harnesh/sessions/`. Direct user shell events record a sequence number, stable event ID, command, initial and final directories, timestamps, exit status, output size, and an output reference. Exact output is stored in mode `0600` blobs. Session directories use mode `0700`.

Before each successful Codex turn, Harnesh sends only direct shell events that the adapter has not seen. Prompt events and Codex-requested actions do not re-enter this synchronization batch. Large outputs use a 32 KiB head and 32 KiB tail projection, with an exact `harnesh history output` reference. One synchronization batch is limited to 256 KiB and has a stable batch ID, so a failed turn can retry the same context without advancing its cursor.

Codex itself remains the authority for conversational context and compaction. Harnesh stores the Codex thread ID and the shell-event cursor. It does not copy or reinterpret the Codex transcript.

Unsupported shells still run through the PTY, but Harnesh can only keep a bounded raw transcript for them. Structured command boundaries, shared-shell agent actions, prompt hooks, and exact event history currently require Bash or Fish.

## Configuration

- `HARNESH_AGENT_BIN`: executable used in place of `agent`
- `HARNESH_ACTION_LIMIT`: positive shared-shell action limit; default `32`
- `XDG_STATE_HOME`: root for durable Harnesh session state
- `SHELL`: interactive shell executable; Harnesh falls back to `bash` when unset

Harnesh starts Codex with the equivalent of:

```console
agent --here codex exec --json --output-schema SCHEMA --output-last-message FILE -
```

Later turns use `codex exec resume THREAD_ID` through the same wrapper. `--here` is required because the live shell already owns the selected project and directory.

## Development

Run the complete gate:

```console
$ make test
```

The gate runs Go tests, a deterministic fake-`agent` tmux test, and `nix flake check`. The end-to-end test covers native thread resume, multiple shared-shell actions, failure status, working-directory and variable state, aliases, history, direct-event synchronization, exact output retrieval, and restart behavior.
