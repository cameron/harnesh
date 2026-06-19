package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolvePiCommandUsesOverride(t *testing.T) {
	dir := t.TempDir()
	fakePi := filepath.Join(dir, "pi")
	if err := os.WriteFile(fakePi, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ATY_PI_BIN", fakePi)

	cmd, err := resolvePiCommand()
	if err != nil {
		t.Fatalf("resolvePiCommand returned error: %v", err)
	}
	if cmd.name != fakePi {
		t.Fatalf("resolvePiCommand name = %q, want %q", cmd.name, fakePi)
	}
	if len(cmd.args) != 0 {
		t.Fatalf("resolvePiCommand args = %v, want none", cmd.args)
	}
}

func TestResolvePiCommandFallsBackToNpmExec(t *testing.T) {
	dir := t.TempDir()
	fakeNpm := filepath.Join(dir, "npm")
	if err := os.WriteFile(fakeNpm, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ATY_PI_BIN", "")
	t.Setenv("PATH", dir)

	cmd, err := resolvePiCommand()
	if err != nil {
		t.Fatalf("resolvePiCommand returned error: %v", err)
	}
	if cmd.name != fakeNpm {
		t.Fatalf("resolvePiCommand name = %q, want %q", cmd.name, fakeNpm)
	}
	got := strings.Join(cmd.args, " ")
	want := "exec --yes --package @earendil-works/pi-coding-agent -- pi"
	if got != want {
		t.Fatalf("resolvePiCommand args = %q, want %q", got, want)
	}
}

func TestPiArgsUseOllamaModel(t *testing.T) {
	t.Setenv("ATY_OLLAMA_MODEL", "qwen3:4b")

	args := strings.Join(piArgs("hello"), "\n")
	for _, want := range []string{"--no-session", "--model", "ollama/qwen3:4b", "--append-system-prompt", "-p", "hello"} {
		if !strings.Contains(args, want) {
			t.Fatalf("piArgs missing %q in %q", want, args)
		}
	}
	if !strings.Contains(args, "same persistent PTY-backed shell session") {
		t.Fatalf("piArgs system prompt does not describe shared shell execution: %q", args)
	}
	if !strings.Contains(args, `{"action":"bash","action_input":"command to run"}`) {
		t.Fatalf("piArgs system prompt does not describe JSON shell action fallback: %q", args)
	}
}

func TestBuildPiPromptDescribesSharedBashTool(t *testing.T) {
	prompt := buildPiPrompt("run echo hello", "[shell] pwd")
	for _, want := range []string{
		"use the bash tool",
		"same persistent shell session",
		"[shell] pwd",
		"run echo hello",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("buildPiPrompt missing %q in:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "do not mutate the user's persistent shell session") {
		t.Fatalf("buildPiPrompt still contains stale isolated-shell limitation:\n%s", prompt)
	}
}

func TestBuildDeferredShellPiPromptRequiresJSONAction(t *testing.T) {
	prompt := buildDeferredShellPiPrompt("please run ls", "[shell] pwd")
	for _, want := range []string{
		"native tool calls are not available",
		"respond with exactly one JSON object",
		`{"action":"bash","action_input":"command to run"}`,
		"Do not claim you ran a command in prose",
		"please run ls",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("buildDeferredShellPiPrompt missing %q in:\n%s", want, prompt)
		}
	}
}

func TestWriteOllamaModelsConfig(t *testing.T) {
	t.Setenv("ATY_OLLAMA_MODEL", "qwen3:4b")
	path := filepath.Join(t.TempDir(), "models.json")

	if err := writeOllamaModelsConfig(path, "http://127.0.0.1:12345/v1"); err != nil {
		t.Fatalf("writeOllamaModelsConfig returned error: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	for _, want := range []string{
		`"baseUrl": "http://127.0.0.1:12345/v1"`,
		`"api": "openai-completions"`,
		`"apiKey": "ollama"`,
		`"id": "qwen3:4b"`,
		`"supportsDeveloperRole": false`,
		`"supportsReasoningEffort": false`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("models config missing %s in:\n%s", want, text)
		}
	}
}

func TestForegroundShellArgsInstallFishAgentHooks(t *testing.T) {
	args := strings.Join(foregroundShellArgs(shellFish, "/tmp/aty", "/tmp/transcript"), " ")
	for _, want := range []string{
		"function ,",
		"fish_command_not_found",
		"--agent-prompt $argv",
		"set -l __aty_cmd",
		"history append",
		"fish_prompt",
		"printf '%s",
		"eval \"$__aty_cmd\"",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("fish init missing %q in %q", want, args)
		}
	}
}

func TestBashInitInstallsAgentHooks(t *testing.T) {
	rc, err := writeBashInitFile("/tmp/aty", "/tmp/transcript")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(rc)

	content, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	for _, want := range []string{
		"function ,()",
		"command_not_found_handle",
		"--agent-prompt",
		"__aty_cmd=$(",
		"history -s",
		"${PS1@P}",
		"printf '%s\\n'",
		"eval \"$__aty_cmd\"",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("bash init missing %q in:\n%s", want, text)
		}
	}
}

func TestRunPiPromptInvokesPiAndRecordsTranscript(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "args.log")
	fakePi := filepath.Join(dir, "pi")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + shellQuote(logPath) + "\nprintf 'fake pi response\\n'\n"
	if err := os.WriteFile(fakePi, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ATY_PI_BIN", fakePi)
	t.Setenv("ATY_OLLAMA_BASE_URL", "http://127.0.0.1:1/v1")

	tr := &transcript{}
	tr.append("[shell] ", []byte("previous context"))
	runPiPrompt(context.Background(), "hello are you an agent??", tr)

	args, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("fake pi was not invoked: %v", err)
	}
	if !bytes.Contains(args, []byte("-p\n")) {
		t.Fatalf("fake pi args did not include -p: %q", args)
	}
	if !strings.Contains(string(args), "hello are you an agent??") {
		t.Fatalf("fake pi prompt missing user request: %q", args)
	}
	if !strings.Contains(tr.text(), "fake pi response") {
		t.Fatalf("transcript missing fake pi output: %q", tr.text())
	}
}

func TestRunPiPromptUsesExtensionBashWhenShellSocketIsAvailable(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "args.log")
	fakePi := filepath.Join(dir, "pi")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + shellQuote(logPath) + "\nprintf 'fake pi response\\n'\n"
	if err := os.WriteFile(fakePi, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ATY_PI_BIN", fakePi)
	t.Setenv("ATY_OLLAMA_BASE_URL", "http://127.0.0.1:1/v1")
	t.Setenv("ATY_SHELL_SOCKET", filepath.Join(dir, "shell.sock"))

	tr := &transcript{}
	runPiPrompt(context.Background(), "run echo hello", tr)

	args, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("fake pi was not invoked: %v", err)
	}
	text := string(args)
	for _, want := range []string{"-e\n", "aty-shell-extension.ts", "--no-builtin-tools", "--tools\nbash"} {
		if !strings.Contains(text, want) {
			t.Fatalf("fake pi args missing %q in:\n%s", want, text)
		}
	}
}

func TestExtractBashActionFromFencedJSON(t *testing.T) {
	output := "```json\n{\"action\":\"bash\",\"action_input\":\"echo agent-shell-ok\"}\n```"

	command, ok := extractBashAction(output)
	if !ok {
		t.Fatalf("extractBashAction did not find command")
	}
	if command != "echo agent-shell-ok" {
		t.Fatalf("extractBashAction command = %q, want echo agent-shell-ok", command)
	}
}

func TestExtractBashActionFromObjectInput(t *testing.T) {
	output := `{"action":"bash","action_input":{"command":"pwd"}}`

	command, ok := extractBashAction(output)
	if !ok {
		t.Fatalf("extractBashAction did not find command")
	}
	if command != "pwd" {
		t.Fatalf("extractBashAction command = %q, want pwd", command)
	}
}
