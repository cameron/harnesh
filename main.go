package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

const transcriptLimit = 64 * 1024
const defaultOllamaHost = "r720"
const defaultOllamaModel = "gemma4:12b"

type transcript struct {
	mu   sync.Mutex
	buf  []byte
	file *os.File
}

func (t *transcript) append(label string, data []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if label != "" {
		t.buf = append(t.buf, []byte(label)...)
	}
	t.buf = append(t.buf, data...)
	if len(data) == 0 || data[len(data)-1] != '\n' {
		t.buf = append(t.buf, '\n')
	}
	if len(t.buf) > transcriptLimit {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-transcriptLimit:]...)
	}
	if t.file != nil {
		if label != "" {
			_, _ = t.file.WriteString(label)
		}
		_, _ = t.file.Write(data)
		if len(data) == 0 || data[len(data)-1] != '\n' {
			_, _ = t.file.WriteString("\n")
		}
	}
}

func (t *transcript) text() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(append([]byte(nil), t.buf...))
}

type shellExecRequest struct {
	ID      string `json:"id"`
	Command string `json:"command"`
}

type promptRequest struct {
	Prompt string `json:"prompt"`
}

type promptResponse struct {
	Command string `json:"command,omitempty"`
}

type shellExecResponse struct {
	Output   string `json:"output"`
	ExitCode int    `json:"exitCode"`
	Error    string `json:"error,omitempty"`
}

type shellSession struct {
	ptmx       *os.File
	transcript *transcript
	kind       shellKind

	mu       sync.Mutex
	pending  *pendingCommand
	buffer   bytes.Buffer
	execMu   sync.Mutex
	captures map[string]*shellCapture
}

type shellKind string

const (
	shellPOSIX shellKind = "posix"
	shellFish  shellKind = "fish"
)

type pendingCommand struct {
	sentinel string
	done     chan struct{}
}

type shellCapture struct {
	marker   string
	done     chan shellExecResponse
	output   bytes.Buffer
	exitCode int
}

func startShell(tr *transcript) (*shellSession, *exec.Cmd, error) {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}

	kind := detectShellKind(shell)
	cmd := exec.Command(shell, shellArgs(kind)...)
	cmd.Env = append(os.Environ(),
		"TERM=dumb",
		"HARNESH=1",
		"PS1=",
		"PROMPT_COMMAND=",
	)

	ptmx, tty, err := pty.Open()
	if err != nil {
		return nil, nil, err
	}
	if err := disableEcho(tty); err != nil {
		_ = ptmx.Close()
		_ = tty.Close()
		return nil, nil, err
	}
	cmd.Stdin = tty
	cmd.Stdout = tty
	cmd.Stderr = tty
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		_ = ptmx.Close()
		_ = tty.Close()
		return nil, nil, err
	}
	_ = tty.Close()

	session := &shellSession{ptmx: ptmx, transcript: tr, kind: kind, captures: make(map[string]*shellCapture)}
	go session.readLoop()
	return session, cmd, nil
}

func disableEcho(tty *os.File) error {
	termios, err := unix.IoctlGetTermios(int(tty.Fd()), unix.TCGETS)
	if err != nil {
		return err
	}
	termios.Lflag &^= unix.ECHO | unix.ECHOE | unix.ECHOK | unix.ECHONL
	return unix.IoctlSetTermios(int(tty.Fd()), unix.TCSETS, termios)
}

func shellArgs(kind shellKind) []string {
	switch kind {
	case shellFish:
		return []string{"--no-config", "--private", "-c", "while read --null __harnesh_cmd; eval $__harnesh_cmd; end"}
	default:
		return []string{"-c", "while IFS= read -r __harnesh_cmd; do eval \"$__harnesh_cmd\"; done"}
	}
}

func detectShellKind(shell string) shellKind {
	switch filepath.Base(shell) {
	case "fish":
		return shellFish
	default:
		return shellPOSIX
	}
}

func (s *shellSession) close() error {
	return s.ptmx.Close()
}

func (s *shellSession) readLoop() {
	buf := make([]byte, 4096)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			s.handleOutput(buf[:n])
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				fmt.Fprintf(os.Stderr, "\n[harnesh: shell PTY read error: %v]\n", err)
			}
			return
		}
	}
}

func (s *shellSession) handleOutput(chunk []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.buffer.Write(chunk)
	for {
		line, err := s.buffer.ReadBytes('\n')
		if len(line) > 0 {
			if s.handleLineLocked(line) {
				continue
			}
		}
		if err != nil {
			if s.buffer.Len() > 0 {
				pending := append([]byte(nil), s.buffer.Bytes()...)
				s.buffer.Reset()
				if !s.handleLineLocked(pending) {
					s.buffer.Write(pending)
				}
			}
			return
		}
	}
}

func (s *shellSession) handleLineLocked(line []byte) bool {
	trimmed := strings.TrimSpace(string(line))
	for id, capture := range s.captures {
		if strings.HasPrefix(trimmed, capture.marker+":") {
			statusText := strings.TrimPrefix(trimmed, capture.marker+":")
			exitCode, _ := strconv.Atoi(statusText)
			capture.exitCode = exitCode
			delete(s.captures, id)
			capture.done <- shellExecResponse{Output: capture.output.String(), ExitCode: exitCode}
			close(capture.done)
			return true
		}
		capture.output.Write(line)
	}

	if s.pending != nil && strings.HasPrefix(trimmed, s.pending.sentinel+":") {
		close(s.pending.done)
		s.pending = nil
		return true
	}

	os.Stdout.Write(line)
	s.transcript.append("[shell] ", line)
	return true
}

func (s *shellSession) hasCapture() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.captures) > 0
}

func (s *shellSession) handleCapturedOutput(chunk []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.buffer.Write(chunk)
	for {
		line, err := s.buffer.ReadBytes('\n')
		if len(line) > 0 {
			if s.handleCapturedLineLocked(line) {
				continue
			}
		}
		if err != nil {
			return
		}
	}
}

func (s *shellSession) handleCapturedLineLocked(line []byte) bool {
	trimmed := strings.TrimSpace(string(line))
	for id, capture := range s.captures {
		if strings.HasPrefix(trimmed, capture.marker+":") {
			statusText := strings.TrimPrefix(trimmed, capture.marker+":")
			exitCode, _ := strconv.Atoi(statusText)
			delete(s.captures, id)
			capture.done <- shellExecResponse{Output: capture.output.String(), ExitCode: exitCode}
			close(capture.done)
			return true
		}
		capture.output.Write(line)
	}

	_, _ = os.Stdout.Write(line)
	s.transcript.append("[shell] ", line)
	return true
}

func (s *shellSession) exec(command string) {
	command = strings.TrimSpace(command)
	if command == "" {
		return
	}

	s.transcript.append("[user command] ", []byte(command))
	s.execInternal(command)
}

func (s *shellSession) execForAgent(command string) shellExecResponse {
	command = strings.TrimSpace(command)
	if command == "" {
		return shellExecResponse{ExitCode: 0}
	}

	s.execMu.Lock()
	defer s.execMu.Unlock()

	id := fmt.Sprintf("%d", time.Now().UnixNano())
	capture := &shellCapture{
		marker: "__HARNESH_AGENT_STATUS_" + id + "__",
		done:   make(chan shellExecResponse, 1),
	}

	s.mu.Lock()
	s.captures[id] = capture
	s.mu.Unlock()

	s.transcript.append("[agent command] ", []byte(command))
	compound := command + "\n" + s.statusCommand(capture.marker) + "\n"
	if _, err := s.ptmx.Write([]byte(compound)); err != nil {
		s.mu.Lock()
		delete(s.captures, id)
		s.mu.Unlock()
		return shellExecResponse{ExitCode: 1, Error: fmt.Sprintf("failed to write to shell PTY: %v", err)}
	}

	select {
	case result := <-capture.done:
		return result
	case <-time.After(5 * time.Minute):
		s.mu.Lock()
		delete(s.captures, id)
		s.mu.Unlock()
		return shellExecResponse{Output: capture.output.String(), ExitCode: 124, Error: "command timed out waiting for shell marker"}
	}
}

func (s *shellSession) execInternal(command string) {
	sentinel := fmt.Sprintf("__HARNESH_STATUS_%d__", time.Now().UnixNano())
	done := make(chan struct{})

	s.mu.Lock()
	s.pending = &pendingCommand{sentinel: sentinel, done: done}
	s.mu.Unlock()

	compound := command + s.commandTerminator() + s.statusCommand(sentinel) + s.commandTerminator()
	_, err := s.ptmx.Write([]byte(compound))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[harnesh: failed to write to shell PTY: %v]\n", err)
		return
	}

	<-done
}

func (s *shellSession) commandTerminator() string {
	if s.kind == shellFish {
		return "\x00\n"
	}
	return "\n"
}

func (s *shellSession) statusCommand(sentinel string) string {
	switch s.kind {
	case shellFish:
		return fmt.Sprintf("printf '\\n%s:%%s\\n' $status", sentinel)
	default:
		return fmt.Sprintf("printf '\\n%s:%%s\\n' \"$?\"", sentinel)
	}
}

type piPromptResult struct {
	Command string
}

func runPiPrompt(ctx context.Context, prompt string, tr *transcript) {
	_ = runPiPromptInternal(ctx, prompt, tr, false)
}

func runPiPromptForShell(ctx context.Context, prompt string, tr *transcript) piPromptResult {
	return runPiPromptInternal(ctx, prompt, tr, true)
}

func runPiPromptInternal(ctx context.Context, prompt string, tr *transcript, deferShellCommand bool) piPromptResult {
	piCmd, err := resolvePiCommand()
	if err != nil {
		msg := fmt.Sprintf("[harnesh: %s]\n", err)
		fmt.Fprint(os.Stderr, msg)
		tr.append("[agent error] ", []byte(msg))
		return piPromptResult{}
	}
	piEnv, cleanup, err := preparePiOllamaEnv(ctx, !deferShellCommand)
	if err != nil {
		msg := fmt.Sprintf("[harnesh: %s]\n", err)
		fmt.Fprint(os.Stderr, msg)
		tr.append("[agent error] ", []byte(msg))
		return piPromptResult{}
	}
	defer cleanup()

	fullPrompt := buildPiPrompt(prompt, tr.text())
	if deferShellCommand {
		fullPrompt = buildDeferredShellPiPrompt(prompt, tr.text())
	}
	tr.append("[user prompt] ", []byte(prompt))

	args := append([]string{}, piCmd.args...)
	if extensionPath := envValue(piEnv, "HARNESH_PI_EXTENSION"); extensionPath != "" {
		args = append(args, "-e", extensionPath, "--no-builtin-tools", "--tools", "bash")
	}
	args = append(args, piArgs(fullPrompt)...)
	cmd := exec.CommandContext(ctx, piCmd.name, args...)
	cmd.Env = append(os.Environ(), piEnv...)
	shellSocketPath := envValue(piEnv, "HARNESH_SHELL_SOCKET")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[harnesh: failed to open pi stdout: %v]\n", err)
		return piPromptResult{}
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[harnesh: failed to open pi stderr: %v]\n", err)
		return piPromptResult{}
	}

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "[harnesh: failed to start pi -p: %v]\n", err)
		return piPromptResult{}
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go streamPipe(&wg, stderr, os.Stderr, "[agent stderr] ", tr)
	var stdoutBuf bytes.Buffer
	bufferStdout := shellSocketPath != "" || deferShellCommand
	if !bufferStdout {
		wg.Add(1)
		go streamPipe(&wg, stdout, os.Stdout, "[agent] ", tr)
	} else {
		_, _ = io.Copy(&stdoutBuf, stdout)
	}
	wg.Wait()

	if err := cmd.Wait(); err != nil {
		fmt.Fprintf(os.Stderr, "\n[harnesh: pi -p exited with error: %v]\n", err)
	}

	if !bufferStdout {
		return piPromptResult{}
	}
	output := stdoutBuf.String()
	if command, ok := extractBashAction(output); ok {
		tr.append("[agent action] ", []byte("bash: "+command))
		if deferShellCommand {
			return piPromptResult{Command: command}
		}
		resp, err := executeViaShellSocket(shellSocketPath, command)
		if err != nil {
			msg := fmt.Sprintf("[harnesh: failed to execute agent shell command: %v]\n", err)
			fmt.Fprint(os.Stderr, msg)
			tr.append("[agent error] ", []byte(msg))
			return piPromptResult{}
		}
		if resp.Error != "" {
			msg := fmt.Sprintf("[harnesh: agent shell command failed: %s]\n", resp.Error)
			fmt.Fprint(os.Stderr, msg)
			tr.append("[agent error] ", []byte(msg))
		}
		return piPromptResult{}
	}
	if output != "" {
		_, _ = os.Stdout.Write([]byte(output))
		tr.append("[agent] ", []byte(output))
	}
	return piPromptResult{}
}

func piArgs(prompt string) []string {
	model := envOrDefault("HARNESH_OLLAMA_MODEL", defaultOllamaModel)
	return []string{
		"--no-session",
		"--model", "ollama/" + model,
		"--append-system-prompt", harneshSystemPrompt(),
		"-p", prompt,
	}
}

func harneshSystemPrompt() string {
	return strings.TrimSpace(`You are running inside harnesh, a hybrid shell/agent terminal prototype.

When a user asks you to run, check, inspect, execute, list, print, change directories, set variables, define aliases, or otherwise perform a shell operation, use the bash tool.

In harnesh, the bash tool is not an isolated subprocess. It sends commands to the same persistent PTY-backed shell session that the user is interacting with. Commands and output are visible in the terminal transcript, and shell state such as cwd, variables, aliases, and functions belongs to that shared session.

If native tool calls are unavailable and you need to run a shell command, emit only this JSON shape and no prose: {"action":"bash","action_input":"command to run"}.

Do not say a shell command was run unless you used the bash tool or emitted the JSON action for harnesh to run it.`)
}

type piCommand struct {
	name string
	args []string
}

func resolvePiCommand() (piCommand, error) {
	if override := os.Getenv("HARNESH_PI_BIN"); override != "" {
		if _, err := os.Stat(override); err != nil {
			return piCommand{}, fmt.Errorf("HARNESH_PI_BIN is set but not executable: %s", override)
		}
		return piCommand{name: override}, nil
	}

	if path, err := exec.LookPath("pi"); err == nil {
		return piCommand{name: path}, nil
	}

	if npm, err := exec.LookPath("npm"); err == nil {
		return piCommand{
			name: npm,
			args: []string{"exec", "--yes", "--package", "@earendil-works/pi-coding-agent", "--", "pi"},
		}, nil
	}

	return piCommand{}, fmt.Errorf("pi executable not found on PATH and npm is unavailable for fallback install")
}

func preparePiOllamaEnv(ctx context.Context, enableShellExtension bool) ([]string, func(), error) {
	baseURL := os.Getenv("HARNESH_OLLAMA_BASE_URL")
	var cleanup func()
	if baseURL == "" {
		url, tunnelCleanup, err := ensureOllamaBaseURL(ctx)
		if err != nil {
			return nil, func() {}, err
		}
		baseURL = url
		cleanup = tunnelCleanup
	} else {
		cleanup = func() {}
	}

	configDir, err := os.MkdirTemp("", "harnesh-pi-")
	if err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("failed to create Pi config dir: %w", err)
	}

	modelsPath := filepath.Join(configDir, "models.json")
	if err := writeOllamaModelsConfig(modelsPath, baseURL); err != nil {
		cleanup()
		_ = os.RemoveAll(configDir)
		return nil, func() {}, err
	}
	extensionPath := filepath.Join(configDir, "harnesh-shell-extension.ts")
	if socketPath := os.Getenv("HARNESH_SHELL_SOCKET"); enableShellExtension && socketPath != "" {
		if err := writeHarneshShellExtension(extensionPath, socketPath); err != nil {
			cleanup()
			_ = os.RemoveAll(configDir)
			return nil, func() {}, fmt.Errorf("failed to write harnesh shell Pi extension: %w", err)
		}
	}

	env := []string{
		"PI_CODING_AGENT_DIR=" + configDir,
		"PI_CODING_AGENT_SESSION_DIR=" + filepath.Join(configDir, "sessions"),
	}
	if socketPath := os.Getenv("HARNESH_SHELL_SOCKET"); enableShellExtension && socketPath != "" {
		env = append(env, "HARNESH_PI_EXTENSION="+extensionPath, "HARNESH_SHELL_SOCKET="+socketPath)
	}
	return env, func() {
		cleanup()
		_ = os.RemoveAll(configDir)
	}, nil
}

func writeHarneshShellExtension(path, socketPath string) error {
	source := `import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";
import net from "node:net";

type ShellResponse = { output: string; exitCode: number; error?: string };

function runSharedShell(command: string): Promise<ShellResponse> {
  const socketPath = process.env.HARNESH_SHELL_SOCKET;
  if (!socketPath) {
    return Promise.resolve({ output: "", exitCode: 1, error: "HARNESH_SHELL_SOCKET is not set" });
  }
  return new Promise((resolve) => {
    const socket = net.createConnection(socketPath);
    let data = "";
    socket.setEncoding("utf8");
    socket.on("connect", () => {
      socket.end(JSON.stringify({ command }) + "\n");
    });
    socket.on("data", (chunk) => {
      data += chunk;
    });
    socket.on("error", (err) => {
      resolve({ output: "", exitCode: 1, error: err.message });
    });
    socket.on("close", () => {
      if (!data.trim()) return resolve({ output: "", exitCode: 1, error: "empty response from harnesh shell socket" });
      try {
        resolve(JSON.parse(data) as ShellResponse);
      } catch (err) {
        resolve({ output: data, exitCode: 1, error: err instanceof Error ? err.message : String(err) });
      }
    });
  });
}

export default function (pi: ExtensionAPI) {
  pi.registerTool({
    name: "bash",
    label: "bash",
    description: "Execute a shell command in the same persistent shell session the user is interacting with. Returns stdout and stderr.",
    promptSnippet: "Execute shell commands in the user's shared harnesh shell session.",
    promptGuidelines: [
      "Use bash for shell commands that should share state with the user's terminal session.",
      "The bash tool runs in the user's visible harnesh shell; commands and output appear in the terminal transcript."
    ],
    parameters: Type.Object({
      command: Type.String({ description: "Shell command to execute" }),
      timeout: Type.Optional(Type.Number({ description: "Ignored by harnesh prototype" })),
    }),
    executionMode: "sequential",
    async execute(_toolCallId, params) {
      const result = await runSharedShell(params.command);
      const text = result.output?.trimEnd() || "(no output)";
      if (result.error || result.exitCode !== 0) {
        throw new Error(text + "\n\nCommand exited with code " + result.exitCode + (result.error ? ": " + result.error : ""));
      }
      return { content: [{ type: "text", text }], details: undefined };
    },
  });
}
`
	return os.WriteFile(path, []byte(source), 0o600)
}

func startShellExecServer(shell *shellSession) (string, func(), error) {
	dir := ""
	socketPath := os.Getenv("HARNESH_SHELL_SOCKET_PATH")
	if socketPath == "" {
		var err error
		dir, err = os.MkdirTemp("", "harnesh-shell-")
		if err != nil {
			return "", func() {}, err
		}
		socketPath = filepath.Join(dir, "shell.sock")
	} else {
		_ = os.Remove(socketPath)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		if dir != "" {
			_ = os.RemoveAll(dir)
		}
		return "", func() {}, err
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go handleShellExecConn(conn, shell)
		}
	}()

	cleanup := func() {
		_ = listener.Close()
		<-done
		if dir != "" {
			_ = os.RemoveAll(dir)
		} else {
			_ = os.Remove(socketPath)
		}
	}
	return socketPath, cleanup, nil
}

func startPromptServer(tr *transcript) (string, func(), error) {
	dir, err := os.MkdirTemp("", "harnesh-prompt-")
	if err != nil {
		return "", func() {}, err
	}
	socketPath := filepath.Join(dir, "prompt.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", func() {}, err
	}

	done := make(chan struct{})
	var runMu sync.Mutex
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				var req promptRequest
				if err := json.NewDecoder(conn).Decode(&req); err != nil {
					fmt.Fprintf(os.Stderr, "\nharnesh: failed to read prompt request: %v\n", err)
					return
				}
				runMu.Lock()
				defer runMu.Unlock()
				result := runPiPromptForShell(context.Background(), req.Prompt, tr)
				if err := json.NewEncoder(conn).Encode(promptResponse{Command: result.Command}); err != nil {
					fmt.Fprintf(os.Stderr, "\nharnesh: failed to write prompt response: %v\n", err)
				}
			}()
		}
	}()

	cleanup := func() {
		_ = listener.Close()
		<-done
		_ = os.RemoveAll(dir)
	}
	return socketPath, cleanup, nil
}

func handleShellExecConn(conn net.Conn, shell *shellSession) {
	defer conn.Close()
	reader := textproto.NewReader(bufio.NewReader(conn))
	line, err := reader.ReadLine()
	if err != nil {
		_ = json.NewEncoder(conn).Encode(shellExecResponse{ExitCode: 1, Error: err.Error()})
		return
	}
	var req shellExecRequest
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		_ = json.NewEncoder(conn).Encode(shellExecResponse{ExitCode: 1, Error: err.Error()})
		return
	}
	resp := shell.execForAgent(req.Command)
	_ = json.NewEncoder(conn).Encode(resp)
}

func ensureOllamaBaseURL(ctx context.Context) (string, func(), error) {
	if ollamaReachable(ctx, "http://127.0.0.1:11434") {
		return "http://127.0.0.1:11434/v1", func() {}, nil
	}

	port, err := freeLocalPort()
	if err != nil {
		return "", func() {}, fmt.Errorf("failed to allocate local Ollama tunnel port: %w", err)
	}

	host := envOrDefault("HARNESH_OLLAMA_SSH_HOST", defaultOllamaHost)
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return "", func() {}, fmt.Errorf("ssh is required to reach Ollama on %s", host)
	}

	cmd := exec.CommandContext(ctx, sshPath,
		"-N",
		"-o", "ExitOnForwardFailure=yes",
		"-L", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:11434", port),
		host,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", func() {}, fmt.Errorf("failed to start SSH tunnel to %s: %w", host, err)
	}

	cleanup := func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}

	rootURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ollamaReachable(ctx, rootURL) {
			return rootURL + "/v1", cleanup, nil
		}
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	cleanup()
	msg := strings.TrimSpace(stderr.String())
	if msg == "" {
		msg = "timeout waiting for tunnel"
	}
	return "", func() {}, fmt.Errorf("failed to connect to Ollama on %s via SSH: %s", host, msg)
}

func ollamaReachable(ctx context.Context, rootURL string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rootURL+"/api/tags", nil)
	if err != nil {
		return false
	}
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func freeLocalPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	_, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(portText)
}

func writeOllamaModelsConfig(path, baseURL string) error {
	model := envOrDefault("HARNESH_OLLAMA_MODEL", defaultOllamaModel)
	config := map[string]any{
		"providers": map[string]any{
			"ollama": map[string]any{
				"baseUrl": baseURL,
				"api":     "openai-completions",
				"apiKey":  "ollama",
				"compat": map[string]any{
					"supportsDeveloperRole":     false,
					"supportsReasoningEffort":   false,
					"supportsParallelToolCalls": false,
				},
				"models": []map[string]any{
					{
						"id":            model,
						"name":          model + " (R720 Ollama)",
						"reasoning":     false,
						"input":         []string{"text"},
						"contextWindow": 262144,
						"maxTokens":     32768,
						"cost": map[string]float64{
							"input":      0,
							"output":     0,
							"cacheRead":  0,
							"cacheWrite": 0,
						},
					},
				},
			},
		},
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to encode Pi Ollama models config: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("failed to write Pi Ollama models config: %w", err)
	}
	return nil
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envValue(env []string, name string) string {
	prefix := name + "="
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			return strings.TrimPrefix(item, prefix)
		}
	}
	return ""
}

func streamPipe(wg *sync.WaitGroup, r io.Reader, w io.Writer, label string, tr *transcript) {
	defer wg.Done()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	for scanner.Scan() {
		line := append(scanner.Bytes(), '\n')
		w.Write(line)
		tr.append(label, line)
	}
}

func buildPiPrompt(userPrompt, recent string) string {
	return strings.TrimSpace(fmt.Sprintf(`You are being invoked from harnesh, a prototype hybrid shell/agent terminal.

The recent terminal context below is plain transcript text. If you need to run a shell command, use the bash tool. In harnesh, that tool sends the command to the same persistent shell session shown in the transcript.

Recent terminal context:
%s

User request:
%s`, recent, userPrompt))
}

func buildDeferredShellPiPrompt(userPrompt, recent string) string {
	return strings.TrimSpace(fmt.Sprintf(`You are being invoked from harnesh, a prototype hybrid shell/agent terminal.

Your stdout is being parsed by harnesh. In this invocation, native tool calls are not available.

If the user's request should run, inspect, list, print, check, execute, or otherwise use a shell command, respond with exactly one JSON object and no markdown, no code fence, and no explanatory prose:
{"action":"bash","action_input":"command to run"}

Use the shortest shell command that satisfies the request. Do not claim you ran a command in prose. Do not include command output; harnesh will run the command in the user's real shell after your JSON response.

If no shell command is appropriate, answer normally.

Recent terminal context:
%s

User request:
%s`, recent, userPrompt))
}

func classifyBareLine(line string) bool {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return false
	}

	for _, token := range fields[1:] {
		if proseStopword(token) {
			return false
		}
	}

	head := fields[0]
	if strings.ContainsAny(head, "/.") && (strings.HasPrefix(head, ".") || strings.HasPrefix(head, "/")) {
		return true
	}
	if isKnownCommand(head) {
		return commandLikeArgs(fields[1:])
	}
	if _, err := exec.LookPath(head); err == nil {
		return commandLikeArgs(fields[1:])
	}
	return false
}

func proseStopword(token string) bool {
	token = strings.ToLower(strings.Trim(token, ".,?!:;\"'()[]{}"))
	switch token {
	case "it", "the", "a", "an", "my", "this", "that", "for", "with", "to", "me", "our", "your":
		return true
	default:
		return false
	}
}

func commandLikeArgs(args []string) bool {
	if len(args) == 0 {
		return true
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") ||
			strings.HasPrefix(arg, ".") ||
			strings.HasPrefix(arg, "/") ||
			strings.ContainsAny(arg, "*?=[]{}:$") ||
			fileExists(arg) {
			continue
		}
		if isCommonSubcommand(arg) {
			continue
		}
		return false
	}
	return true
}

func isKnownCommand(name string) bool {
	switch name {
	case "alias", "bg", "cd", "command", "dirs", "echo", "exit", "export", "fg", "jobs", "popd", "pushd", "pwd",
		"source", "type", "unalias", "unset",
		"cat", "cp", "find", "git", "go", "grep", "head", "less", "ls", "make", "mkdir", "mv", "npm", "pnpm",
		"python", "python3", "rm", "rmdir", "sed", "tail", "touch", "vim", "vi", "yarn":
		return true
	default:
		return false
	}
}

func isCommonSubcommand(arg string) bool {
	switch arg {
	case "add", "build", "check", "clean", "diff", "fmt", "install", "log", "run", "show", "status", "test":
		return true
	default:
		return false
	}
}

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	if strings.HasPrefix(path, "~") {
		home, err := os.UserHomeDir()
		if err == nil {
			path = filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	_, err := os.Stat(path)
	return err == nil
}

func runAgentPromptFromShell(args []string) int {
	prompt := strings.TrimSpace(strings.Join(args, " "))
	if prompt == "" {
		fmt.Fprintln(os.Stderr, "usage: , <prompt>")
		return 2
	}
	if socketPath := os.Getenv("HARNESH_PROMPT_SOCKET"); socketPath != "" {
		command, err := sendPromptToParent(socketPath, prompt)
		if err != nil {
			fmt.Fprintf(os.Stderr, "harnesh: failed to send prompt to parent: %v\n", err)
			return 1
		}
		if command != "" {
			fmt.Print(command)
		}
		return 0
	}
	tr := &transcript{}
	if path := os.Getenv("HARNESH_TRANSCRIPT_FILE"); path != "" {
		if data, err := os.ReadFile(path); err == nil {
			tr.append("", data)
		}
	}
	runPiPrompt(context.Background(), prompt, tr)
	return 0
}

func sendPromptToParent(socketPath, prompt string) (string, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(promptRequest{Prompt: prompt}); err != nil {
		return "", err
	}
	var resp promptResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return "", err
	}
	return resp.Command, nil
}

var fencedJSONActionPattern = regexp.MustCompile("(?s)```json\\s*(\\{.*?\\})\\s*```")

func extractBashAction(output string) (string, bool) {
	candidates := []string{strings.TrimSpace(output)}
	for _, match := range fencedJSONActionPattern.FindAllStringSubmatch(output, -1) {
		if len(match) > 1 {
			candidates = append([]string{match[1]}, candidates...)
		}
	}
	for _, candidate := range candidates {
		if command, ok := parseBashActionJSON(candidate); ok {
			return command, true
		}
	}
	return "", false
}

func parseBashActionJSON(text string) (string, bool) {
	var action struct {
		Action      string          `json:"action"`
		ActionInput json.RawMessage `json:"action_input"`
	}
	if err := json.Unmarshal([]byte(text), &action); err != nil {
		return "", false
	}
	if action.Action != "bash" || len(action.ActionInput) == 0 {
		return "", false
	}
	var command string
	if err := json.Unmarshal(action.ActionInput, &command); err == nil {
		command = strings.TrimSpace(command)
		return command, command != ""
	}
	var input struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(action.ActionInput, &input); err != nil {
		return "", false
	}
	command = strings.TrimSpace(input.Command)
	return command, command != ""
}

func executeViaShellSocket(socketPath, command string) (shellExecResponse, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return shellExecResponse{}, err
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(shellExecRequest{Command: command}); err != nil {
		return shellExecResponse{}, err
	}
	var resp shellExecResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return shellExecResponse{}, err
	}
	return resp, nil
}

func startForegroundShell(tr *transcript, transcriptPath, shellSocketPath, promptSocketPath string) (*exec.Cmd, *os.File, func(), error) {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	kind := detectShellKind(shell)
	exe, err := os.Executable()
	if err != nil {
		return nil, nil, nil, err
	}

	cmd := exec.Command(shell, foregroundShellArgs(kind, exe, transcriptPath)...)
	cmd.Env = append(os.Environ(),
		"HARNESH=1",
		"HARNESH_BIN="+exe,
		"HARNESH_TRANSCRIPT_FILE="+transcriptPath,
		"HARNESH_SHELL_SOCKET="+shellSocketPath,
		"HARNESH_PROMPT_SOCKET="+promptSocketPath,
	)

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, nil, nil, err
	}
	cleanupFiles := func() {}
	return cmd, ptmx, cleanupFiles, nil
}

func foregroundShellArgs(kind shellKind, exe, transcriptPath string) []string {
	switch kind {
	case shellFish:
		agentCall := "HARNESH_TRANSCRIPT_FILE=" + fishQuote(transcriptPath) + " " + fishQuote(exe) + " --agent-prompt"
		init := "function ,; " +
			"set -l __harnesh_cmd (" + agentCall + " $argv); " +
			"if test -n \"$__harnesh_cmd\"; history append -- \"$__harnesh_cmd\"; fish_prompt; printf '%s\n' \"$__harnesh_cmd\"; eval \"$__harnesh_cmd\"; end; " +
			"end; " +
			"function fish_command_not_found --on-event fish_command_not_found; " +
			"set -l __harnesh_cmd (" + agentCall + " $argv); " +
			"if test -n \"$__harnesh_cmd\"; history append -- \"$__harnesh_cmd\"; fish_prompt; printf '%s\n' \"$__harnesh_cmd\"; eval \"$__harnesh_cmd\"; end; " +
			"end"
		return []string{"-C", init}
	default:
		rc, err := writeBashInitFile(exe, transcriptPath)
		if err == nil {
			return []string{"--init-file", rc, "-i"}
		}
		return nil
	}
}

func writeBashInitFile(exe, transcriptPath string) (string, error) {
	file, err := os.CreateTemp("", "harnesh-bashrc-")
	if err != nil {
		return "", err
	}
	defer file.Close()
	home, _ := os.UserHomeDir()
	if home != "" {
		fmt.Fprintf(file, "test -f %s && source %s\n", shellQuote(filepath.Join(home, ".bashrc")), shellQuote(filepath.Join(home, ".bashrc")))
	}
	fmt.Fprintf(file, "function ,(){ local __harnesh_cmd; __harnesh_cmd=$(HARNESH_TRANSCRIPT_FILE=%s %s --agent-prompt \"$*\"); if [ -n \"$__harnesh_cmd\" ]; then history -s \"$__harnesh_cmd\"; printf '%%s' \"${PS1@P}\"; printf '%%s\\n' \"$__harnesh_cmd\"; eval \"$__harnesh_cmd\"; fi; }\n", shellQuote(transcriptPath), shellQuote(exe))
	fmt.Fprintf(file, "function command_not_found_handle(){ local __harnesh_cmd; __harnesh_cmd=$(HARNESH_TRANSCRIPT_FILE=%s %s --agent-prompt \"$*\"); if [ -n \"$__harnesh_cmd\" ]; then history -s \"$__harnesh_cmd\"; printf '%%s' \"${PS1@P}\"; printf '%%s\\n' \"$__harnesh_cmd\"; eval \"$__harnesh_cmd\"; fi; }\n", shellQuote(transcriptPath), shellQuote(exe))
	return file.Name(), nil
}

func fishQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "\\'") + "'"
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func makeRaw(fd int) (func(), error) {
	termios, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return func() {}, err
	}
	original := *termios
	raw := *termios
	raw.Iflag &^= unix.BRKINT | unix.ICRNL | unix.INPCK | unix.ISTRIP | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Cflag |= unix.CS8
	raw.Lflag &^= unix.ECHO | unix.ICANON | unix.IEXTEN | unix.ISIG
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &raw); err != nil {
		return func() {}, err
	}
	return func() {
		_ = unix.IoctlSetTermios(fd, unix.TCSETS, &original)
	}, nil
}

func runForegroundShell() error {
	transcriptFile, err := os.CreateTemp("", "harnesh-transcript-")
	if err != nil {
		return err
	}
	defer os.Remove(transcriptFile.Name())
	defer transcriptFile.Close()

	tr := &transcript{file: transcriptFile}
	dummyShell := &shellSession{transcript: tr}
	socketPath, cleanupSocket, err := startShellExecServer(dummyShell)
	if err != nil {
		return err
	}
	defer cleanupSocket()
	restoreShellSocketEnv := setenvForProcess("HARNESH_SHELL_SOCKET", socketPath)
	defer restoreShellSocketEnv()
	promptSocketPath, cleanupPromptSocket, err := startPromptServer(tr)
	if err != nil {
		return err
	}
	defer cleanupPromptSocket()

	cmd, ptmx, cleanupFiles, err := startForegroundShell(tr, transcriptFile.Name(), socketPath, promptSocketPath)
	if err != nil {
		return err
	}
	dummyShell.ptmx = ptmx
	dummyShell.kind = detectShellKind(envOrDefault("SHELL", "/bin/sh"))
	dummyShell.captures = make(map[string]*shellCapture)
	defer cleanupFiles()
	defer ptmx.Close()

	restore, err := makeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return err
	}
	defer restore()

	var copyWG sync.WaitGroup
	copyWG.Add(2)
	go func() {
		defer copyWG.Done()
		_, _ = io.Copy(ptmx, os.Stdin)
	}()
	go func() {
		defer copyWG.Done()
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				if dummyShell.hasCapture() {
					dummyShell.handleCapturedOutput(chunk)
				} else {
					_, _ = os.Stdout.Write(chunk)
					tr.append("[shell] ", chunk)
				}
			}
			if err != nil {
				return
			}
		}
	}()

	waitErr := cmd.Wait()
	_ = ptmx.Close()
	copyWG.Wait()
	return waitErr
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--agent-prompt" {
		os.Exit(runAgentPromptFromShell(os.Args[2:]))
	}

	if err := runForegroundShell(); err != nil {
		fmt.Fprintf(os.Stderr, "harnesh: %v\n", err)
		os.Exit(1)
	}
}

func setenvForProcess(name, value string) func() {
	previous, hadPrevious := os.LookupEnv(name)
	_ = os.Setenv(name, value)
	return func() {
		if hadPrevious {
			_ = os.Setenv(name, previous)
		} else {
			_ = os.Unsetenv(name)
		}
	}
}
