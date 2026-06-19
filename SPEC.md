# Hybrid Agent-Shell Harness — Implementation Spec (Rough Draft Target)

> Status: design spec for a first rough-draft implementation. Working name TBD.
> Audience: an implementing agent. Make reasonable choices where unspecified and
> note them; the rationale sections exist so you pick consistently with intent.

## 1. Concept

A single terminal interface that is **simultaneously a user-facing shell and an
agent harness**. The user types natural-language prompts and shell commands into
the *same* prompt, side by side. The system is **agent-primary**: the agent is
the default addressee, but familiar shell commands must remain frictionless.

Core thesis: move away from requiring syntactically correct input by default.
The interface tolerates either kind of input and routes it correctly, instead of
forcing the user to declare mode up front.

## 2. Transport layer — PTY

Use a **pseudoterminal (PTY)** as the transport. Allocate a master/slave pair
(`forkpty`/`openpty` or a binding) and run the user's **real `$SHELL`** under the
slave. The harness holds the master.

- **Do NOT implement a custom shell / command parser.** Wrapping the real shell
  preserves the user's aliases, functions, completions, prompt, history, and
  plugins. A bespoke shell throws all of that away and reinvents a worse one.
- Everything that drives the session — human keystrokes forwarded from the
  frontend AND agent-initiated commands — is just a **write to the master**.
  This unification is the reason PTY is the right primitive (see §7).
- A real tty (not a pipe) is required so interactive programs behave correctly
  (buffering, color, prompts, readline, job control, full-screen apps).

Suggested libraries (pick per chosen language): `creack/pty` (Go),
`portable-pty` (Rust), `node-pty` (Node), `pty`/`ptyprocess` (Python).

### 2.1 Input/output separation nuance

The master interleaves program output with the **echo of input** (the line
discipline echoes typed input when echo is on). For a clean input vs output
transcript:
- Track the bytes you *write* to the master as the "input" channel yourself, and
  treat master *reads* as "output" (the echoed copy will still appear there); or
- Run the slave with echo off and perform echo at your own layer.

## 3. Shell-agnosticism strategy

Split "agnostic" into two layers:

- **Transport layer — agnostic for free.** Any `$SHELL` runs under the PTY
  (bash, zsh, fish, nu, pwsh, …); you are just shuttling bytes.
- **Semantic layer — per-shell shim.** Clean command boundaries, exit codes, and
  output demarcation require hooking the shell's prompt, which differs per shell:
  - bash: `PROMPT_COMMAND` / `PS0` / `PS1`
  - zsh: `preexec` / `precmd`
  - fish: event functions (`fish_preexec` / `fish_postexec`)
  - nushell: config hooks
  - pwsh: `prompt` function

Ship ~5 small shims. Shells without a shim **degrade gracefully** to a raw
transcript (no structured boundaries, still fully usable).

### 3.1 Command boundary / exit-code capture

- **Preferred:** emit OSC 133 semantic prompt markers from the shim
  (prompt-start / command-start / command-end-with-exit-code) and parse them out
  of the master stream → structured `(command, output, exit_code)` tuples.
- **Universal fallback (no integration):** sentinel trick. After forwarding a
  command, append a unique marker carrying the status, e.g.
  `; printf '\n\001<MARKER>\001%d\n' $?`, then read the master until the marker.
  Works across the Bourne family; variants needed for fish (`$status`),
  nushell, and pwsh (`$LASTEXITCODE`).

## 4. Input routing model

Agent-primary with **tolerant classification by default** and explicit override
sigils. Three namespaces:

| Input            | Behavior                                                                 |
|------------------|--------------------------------------------------------------------------|
| **bare line**    | Classify (§5). High-confidence command → run directly, **no model**. Otherwise → send to agent as a prompt. |
| **`!` prefix**   | Force **verbatim shell**. Bypass the model and the classifier entirely. Deterministic, low-latency. This is the "I mean this literally" / trust + paste anchor. |
| **`,` prefix**   | Force **prose / prompt**. Override for imperative English that would otherwise classify as a command (`make it faster`, `find the bug`). |
| **`/` prefix**   | Reserved for harness meta-commands (`/model`, `/context`, `/clear`, …).   |

Notes:
- `!` is the catch-all escape for *any* collision (e.g. a tool literally named
  `,`): prefix it and it runs literally. No other special-casing needed.
- Classification is a **routing convenience**, never a silent guess on
  high-stakes input — but see §5.3: there is no destructive-command gate.

## 5. Classifier spec

Goal: decide whether a bare line is a shell command or a prompt. **No ML
required** — wordlist + argument-shape checks + live-environment resolution
handle the overwhelming majority.

### 5.1 Signals

- **Function-word / stopword presence** (high-precision PROSE tell): pronouns,
  articles, prepositions as bare tokens — `it, the, a, my, this, that, for,
  with, to`. Real commands almost never carry these as arguments
  (`make install`, `git add .` have none; `make it faster` has `it`).
- **Argument shape:** do trailing tokens look like flags (`-x` / `--x`), paths,
  files that exist on disk, globs, jobspecs (`%1`), or PIDs? Bare English words
  that are none of these lean prose.
- **argv[0] resolution against the LIVE shell environment**, not just PATH:
  builtins + aliases + functions + history. (`g` is a valid command head only
  because the user aliased it.) This requires a query path into the running
  shell — `type` / `whence` via the shim. **The classifier is coupled to the
  shim, not standalone.**
- **Shell history** as a positive, command-*confirming* signal (asymmetric:
  confirms commands, weak as a prose signal).

### 5.2 Bias: conservative-toward-agent (fail-open)

Misrouting a command to the agent is **benign** — the agent runs and/or explains
it, which is exactly the desired behavior for unfamiliar commands. The only thing
to avoid is **prose silently executing as a command**. Therefore: only fast-path
to direct execution on **high confidence**; everything ambiguous goes to the
agent.

### 5.3 No destructive-command gate (explicit non-goal)

A confidently-classified command runs regardless of how destructive it is —
`rm -rf build` runs exactly like `ls`. Do **not** require a sigil/confirm for
dangerous commands; that would tax the most fluent muscle-memory usage to defend
against a near-empty set (prose rarely produces the arg shapes — PIDs, flags,
real paths — that destructive commands need, and near-misses carry the function
words already filtered in §5.1).

The only safety net is the one already in the design: genuinely **ambiguous /
low-confidence** input fails open to the agent, which can surface "did you mean
to run X?" on its own. Destructiveness earns no special rule.

## 6. Interactivity passthrough (critical)

When a foreground child program owns the tty in raw/cbreak mode (vim, less, top,
ssh, password prompts), **bare input is no longer a prompt** — it is keystrokes
for that program.

- The harness must **suspend classification / prompt-interpretation** and forward
  raw bytes while such a program is in the foreground, then resume on exit.
- **Detection:** watch the tty foreground process group (`tcgetpgrp` on the
  slave) and/or the slave's termios mode (raw vs cooked). When a non-shell child
  holds the foreground pgrp, or the tty is in raw mode, switch to raw passthrough.
- Plain terminal emulators avoid this because they always forward raw; this
  harness layers semantics on top, so it must know precisely when to turn the
  semantics OFF.

## 7. Unified execution & transcript

There are two sources of shell execution:
1. **User-initiated** — `!` lines and classified-as-command bare lines.
2. **Agent-initiated** — the agent's tool calls.

**Both must route through the same PTY master and the same transcript.** If the
agent gets a separate exec path, the agent's view and the user's view desync (the
agent won't see the effects of the user's commands and vice versa). Funneling both
as "writes to the master" keeps a single shared context — this is the payoff of
choosing the PTY.

## 8. Suggested build order

1. PTY transport: spawn `$SHELL` under a PTY, frontend forwards keystrokes, render
   master output. (Baseline = a transparent terminal.)
2. Raw passthrough state machine (§6) — get this right before layering semantics,
   or interactive apps break.
3. Sigil routing (`!`, `,`, `/`) and the bare→agent path.
4. Classifier (§5) + a test corpus of real ambiguous lines (`make it faster`,
   `find the bug`, `kill the server`, `git add .`, `npm run build`, `rm -rf x`)
   to tune signals before more plumbing.
5. Per-shell shim for the user's primary shell (start with one) → OSC 133 or
   sentinel boundaries (§3.1).
6. Agent tool-call execution routed through the shared master (§7).
7. Additional shims; graceful degradation path.

## 9. Non-goals / explicit decisions

- No custom shell implementation (§2).
- No destructive-command confirmation gate (§5.3).
- Not designed around any single ecosystem's tooling (e.g. don't special-case
  niche tools; the `!` verbatim escape already subsumes sigil collisions).

## 10. Open questions to resolve during implementation

- Exact sentinel byte sequence and how to keep it from appearing in normal output.
- How visibly to surface the bare-input classification decision (so the user knows
  when to reach for `,` or `!`) without it becoming noisy.
- Whether `/` meta-commands are handled entirely client-side or can also be
  agent-routed.
- Multi-line / pasted-block handling (pasted commands should hit the deterministic
  path and not be mangled).