package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeBashTestScript(t *testing.T, path, body string) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal("Bash is required for adapter tests")
	}
	script := "#!" + bash + "\n" + body
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestCodexAdapterStartsAndResumesNativeThread(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")
	promptPath := filepath.Join(dir, "prompt.log")
	fakeAgent := filepath.Join(dir, "agent")
	script := `set -euo pipefail
response=
previous=
for arg in "$@"; do
  if [[ "$previous" == --output-last-message ]]; then response="$arg"; fi
  previous="$arg"
done
printf 'cwd=%s\n' "$PWD" >> "$FAKE_AGENT_LOG"
printf 'arg=%s\n' "$@" >> "$FAKE_AGENT_LOG"
cat > "$FAKE_AGENT_PROMPT"
printf '%s\n' '{"kind":"answer","answer":"adapter ok","command":""}' > "$response"
printf '%s\n' '{"type":"thread.started","thread_id":"thread-123"}'
printf '%s\n' '{"type":"turn.completed"}'
`
	writeBashTestScript(t, fakeAgent, script)
	t.Setenv("HARNESH_AGENT_BIN", fakeAgent)
	t.Setenv("FAKE_AGENT_LOG", logPath)
	t.Setenv("FAKE_AGENT_PROMPT", promptPath)
	adapter, err := newCodexAdapter(dir)
	if err != nil {
		t.Fatal(err)
	}
	adapter.progress = &bytes.Buffer{}

	reply, err := adapter.Run(context.Background(), agentTurn{CWD: dir, Prompt: "first prompt"})
	if err != nil {
		t.Fatal(err)
	}
	if reply.ThreadID != "thread-123" || reply.Answer != "adapter ok" || reply.Action != nil {
		t.Fatalf("unexpected start reply: %#v", reply)
	}
	prompt, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(prompt) != "first prompt" {
		t.Fatalf("prompt = %q", prompt)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"cwd=" + dir,
		"arg=--here",
		"arg=codex",
		"arg=exec",
		"arg=--json",
		"arg=--output-schema",
		"arg=--output-last-message",
		"arg=-",
	} {
		if !strings.Contains(string(log), required) {
			t.Fatalf("start log lacks %q:\n%s", required, log)
		}
	}
	if strings.Contains(string(log), "arg=resume") {
		t.Fatalf("new thread unexpectedly resumed:\n%s", log)
	}

	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	reply, err = adapter.Run(context.Background(), agentTurn{CWD: dir, ThreadID: "thread-123", Prompt: "second prompt"})
	if err != nil {
		t.Fatal(err)
	}
	if reply.ThreadID != "thread-123" || reply.Answer != "adapter ok" {
		t.Fatalf("unexpected resume reply: %#v", reply)
	}
	log, _ = os.ReadFile(logPath)
	resumeArgs := string(log)
	if !strings.Contains(resumeArgs, "arg=resume") || !strings.Contains(resumeArgs, "arg=thread-123") {
		t.Fatalf("resume arguments missing:\n%s", resumeArgs)
	}
}

func TestCodexAdapterReturnsShellAction(t *testing.T) {
	dir := t.TempDir()
	fakeAgent := filepath.Join(dir, "agent")
	script := `set -euo pipefail
previous=
for arg in "$@"; do
  if [[ "$previous" == --output-last-message ]]; then response="$arg"; fi
  previous="$arg"
done
printf '%s\n' '{"kind":"shell","answer":"","command":"cd /tmp && pwd"}' > "$response"
printf '%s\n' '{"type":"thread.started","thread_id":"thread-action"}'
`
	writeBashTestScript(t, fakeAgent, script)
	t.Setenv("HARNESH_AGENT_BIN", fakeAgent)
	adapter, err := newCodexAdapter(dir)
	if err != nil {
		t.Fatal(err)
	}
	adapter.progress = &bytes.Buffer{}
	reply, err := adapter.Run(context.Background(), agentTurn{CWD: dir, Prompt: "change directory"})
	if err != nil {
		t.Fatal(err)
	}
	if reply.Action == nil || reply.Action.Command != "cd /tmp && pwd" || !validID(reply.Action.ID) {
		t.Fatalf("unexpected action reply: %#v", reply)
	}
}

func TestCodexAdapterReportsFailedOrMismatchedResume(t *testing.T) {
	dir := t.TempDir()
	fakeAgent := filepath.Join(dir, "agent")
	script := `set -euo pipefail
printf '%s\n' '{"type":"thread.started","thread_id":"different-thread"}'
sleep 30
`
	writeBashTestScript(t, fakeAgent, script)
	t.Setenv("HARNESH_AGENT_BIN", fakeAgent)
	adapter, err := newCodexAdapter(dir)
	if err != nil {
		t.Fatal(err)
	}
	adapter.progress = &bytes.Buffer{}
	_, err = adapter.Run(context.Background(), agentTurn{CWD: dir, ThreadID: "missing-thread", Prompt: "resume"})
	if err == nil || !strings.Contains(err.Error(), "unexpected thread") {
		t.Fatalf("mismatched resume error = %v", err)
	}
}

func TestCodexAdapterRequiresThreadStartedEvent(t *testing.T) {
	dir := t.TempDir()
	fakeAgent := filepath.Join(dir, "agent")
	script := `set -euo pipefail
previous=
for arg in "$@"; do
  if [[ "$previous" == --output-last-message ]]; then response="$arg"; fi
  previous="$arg"
done
printf '%s\n' '{"kind":"answer","answer":"orphaned","command":""}' > "$response"
printf '%s\n' '{"type":"turn.completed"}'
`
	writeBashTestScript(t, fakeAgent, script)
	t.Setenv("HARNESH_AGENT_BIN", fakeAgent)
	adapter, err := newCodexAdapter(dir)
	if err != nil {
		t.Fatal(err)
	}
	adapter.progress = &bytes.Buffer{}
	_, err = adapter.Run(context.Background(), agentTurn{CWD: dir, ThreadID: "missing-native-thread", Prompt: "resume"})
	if err == nil || !strings.Contains(err.Error(), "thread.started") {
		t.Fatalf("missing native thread event error = %v", err)
	}
}

func TestInitialAgentPromptSeparatesShellContextFromRequest(t *testing.T) {
	prompt := initialAgentPrompt("why did that fail?", "<shell-event>false</shell-event>")
	for _, required := range []string{
		"live PTY-backed shell",
		"built-in shell tools",
		"return kind \"shell\"",
		"<shell-event>false</shell-event>",
		"<user-request>\nwhy did that fail?",
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("agent prompt lacks %q:\n%s", required, prompt)
		}
	}
}
