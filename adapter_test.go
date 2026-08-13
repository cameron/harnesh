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

func TestAgentBridgeStartsAndResumesSelectedHarness(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")
	promptPath := filepath.Join(dir, "prompt.log")
	fakeAgent := filepath.Join(dir, "agent")
	writeBashTestScript(t, fakeAgent, `set -euo pipefail
printf 'cwd=%s\n' "$PWD" >> "$FAKE_AGENT_LOG"
printf 'arg=%s\n' "$@" >> "$FAKE_AGENT_LOG"
cat > "$FAKE_AGENT_PROMPT"
printf '%s\n' "$FAKE_AGENT_RESPONSE"
`)
	t.Setenv("HARNESH_AGENT_BIN", fakeAgent)
	t.Setenv("FAKE_AGENT_LOG", logPath)
	t.Setenv("FAKE_AGENT_PROMPT", promptPath)
	t.Setenv("FAKE_AGENT_RESPONSE", `{"harness":"pi","session_id":"pi:session-123","kind":"answer","answer":"adapter ok","command":""}`)
	adapter := newAgentBridge()
	adapter.progress = &bytes.Buffer{}

	reply, err := adapter.Run(context.Background(), agentTurn{CWD: dir, Prompt: "first prompt"})
	if err != nil {
		t.Fatal(err)
	}
	if reply.ThreadID != "pi:session-123" || reply.Answer != "adapter ok" || reply.Action != nil {
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
	for _, required := range []string{"cwd=" + dir, "arg=--here", "arg=--harnesh-turn"} {
		if !strings.Contains(string(log), required) {
			t.Fatalf("start log lacks %q:\n%s", required, log)
		}
	}
	for _, forbidden := range []string{"arg=--session", "arg=codex", "arg=pi", "arg=claude"} {
		if strings.Contains(string(log), forbidden) {
			t.Fatalf("new session log contains %q:\n%s", forbidden, log)
		}
	}

	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	reply, err = adapter.Run(context.Background(), agentTurn{CWD: dir, ThreadID: "pi:session-123", Prompt: "second prompt"})
	if err != nil {
		t.Fatal(err)
	}
	if reply.ThreadID != "pi:session-123" || reply.Answer != "adapter ok" {
		t.Fatalf("unexpected resume reply: %#v", reply)
	}
	log, err = os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"arg=--session", "arg=pi:session-123"} {
		if !strings.Contains(string(log), required) {
			t.Fatalf("resume log lacks %q:\n%s", required, log)
		}
	}
}

func TestAgentBridgeReturnsShellAction(t *testing.T) {
	dir := t.TempDir()
	fakeAgent := filepath.Join(dir, "agent")
	writeBashTestScript(t, fakeAgent, `set -euo pipefail
cat >/dev/null
printf '%s\n' '{"harness":"codex","session_id":"codex:thread-action","kind":"shell","answer":"","command":"cd /tmp && pwd"}'
`)
	t.Setenv("HARNESH_AGENT_BIN", fakeAgent)
	adapter := newAgentBridge()
	adapter.progress = &bytes.Buffer{}
	reply, err := adapter.Run(context.Background(), agentTurn{CWD: dir, Prompt: "change directory"})
	if err != nil {
		t.Fatal(err)
	}
	if reply.ThreadID != "codex:thread-action" || reply.Action == nil || reply.Action.Command != "cd /tmp && pwd" || !validID(reply.Action.ID) {
		t.Fatalf("unexpected action reply: %#v", reply)
	}
}

func TestAgentBridgeRejectsInvalidResponses(t *testing.T) {
	tests := []struct {
		name     string
		response string
		resume   string
		want     string
	}{
		{
			name:     "mismatched resume",
			response: `{"harness":"codex","session_id":"codex:different","kind":"answer","answer":"wrong","command":""}`,
			resume:   "pi:expected",
			want:     "instead of pi:expected",
		},
		{
			name:     "invalid native session",
			response: `{"harness":"pi","session_id":"pi:not/valid","kind":"answer","answer":"wrong","command":""}`,
			want:     "invalid session ID",
		},
		{
			name:     "multiple objects",
			response: "{\"harness\":\"claude\",\"session_id\":\"claude:first\",\"kind\":\"answer\",\"answer\":\"one\",\"command\":\"\"}\n{\"extra\":true}",
			want:     "more than one JSON response",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			fakeAgent := filepath.Join(dir, "agent")
			writeBashTestScript(t, fakeAgent, `set -euo pipefail
cat >/dev/null
printf '%s\n' "$FAKE_AGENT_RESPONSE"
`)
			t.Setenv("HARNESH_AGENT_BIN", fakeAgent)
			t.Setenv("FAKE_AGENT_RESPONSE", test.response)
			adapter := newAgentBridge()
			adapter.progress = &bytes.Buffer{}
			_, err := adapter.Run(context.Background(), agentTurn{CWD: dir, ThreadID: test.resume, Prompt: "prompt"})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v; want text %q", err, test.want)
			}
		})
	}
}

func TestAgentBridgeReportsCommandFailure(t *testing.T) {
	dir := t.TempDir()
	fakeAgent := filepath.Join(dir, "agent")
	writeBashTestScript(t, fakeAgent, `set -euo pipefail
cat >/dev/null
printf '%s\n' 'bridge failure detail'
exit 9
`)
	t.Setenv("HARNESH_AGENT_BIN", fakeAgent)
	adapter := newAgentBridge()
	adapter.progress = &bytes.Buffer{}
	_, err := adapter.Run(context.Background(), agentTurn{CWD: dir, Prompt: "prompt"})
	if err == nil || !strings.Contains(err.Error(), "bridge failure detail") {
		t.Fatalf("command failure error = %v", err)
	}
}

func TestInitialAgentPromptSeparatesShellContextFromRequest(t *testing.T) {
	prompt := initialAgentPrompt("why did that fail?", "<shell-event>false</shell-event>")
	for _, required := range []string{
		"live PTY-backed shell",
		"built-in shell tools",
		"return kind \"shell\"",
		"native agent session",
		"<shell-event>false</shell-event>",
		"<user-request>\nwhy did that fail?",
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("agent prompt lacks %q:\n%s", required, prompt)
		}
	}
}
