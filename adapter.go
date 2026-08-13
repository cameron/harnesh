package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type sharedAction struct {
	ID      string
	Command string
}

type agentReply struct {
	ThreadID string
	Answer   string
	Action   *sharedAction
}

type agentTurn struct {
	CWD      string
	ThreadID string
	Prompt   string
}

type agentAdapter interface {
	Name() string
	Run(context.Context, agentTurn) (agentReply, error)
}

type codexAdapter struct {
	bin      string
	stateDir string
	progress io.Writer
}

type codexStructuredReply struct {
	Kind    string `json:"kind"`
	Answer  string `json:"answer"`
	Command string `json:"command"`
}

const codexOutputSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "properties": {
    "kind": { "type": "string", "enum": ["answer", "shell"] },
    "answer": { "type": "string" },
    "command": { "type": "string" }
  },
  "required": ["kind", "answer", "command"],
  "additionalProperties": false
}`

func newCodexAdapter(stateDir string) (*codexAdapter, error) {
	bin := os.Getenv("HARNESH_AGENT_BIN")
	if bin == "" {
		bin = "agent"
	}
	schemaPath := filepath.Join(stateDir, "codex-output-schema.json")
	if err := writePrivateFile(schemaPath, []byte(codexOutputSchema+"\n")); err != nil {
		return nil, err
	}
	return &codexAdapter{bin: bin, stateDir: stateDir, progress: os.Stderr}, nil
}

func (a *codexAdapter) Name() string { return "codex" }

func (a *codexAdapter) Run(ctx context.Context, turn agentTurn) (agentReply, error) {
	if turn.CWD == "" || turn.Prompt == "" {
		return agentReply{}, errors.New("Codex turn requires cwd and prompt")
	}
	responseFile, err := os.CreateTemp(a.stateDir, ".codex-response-*")
	if err != nil {
		return agentReply{}, err
	}
	responsePath := responseFile.Name()
	if err := responseFile.Chmod(0o600); err != nil {
		_ = responseFile.Close()
		return agentReply{}, err
	}
	if err := responseFile.Close(); err != nil {
		return agentReply{}, err
	}
	defer os.Remove(responsePath)

	schemaPath := filepath.Join(a.stateDir, "codex-output-schema.json")
	args := []string{"--here", "codex", "exec"}
	if turn.ThreadID != "" {
		args = append(args, "resume")
	}
	args = append(args,
		"--json",
		"--skip-git-repo-check",
		"--output-schema", schemaPath,
		"--output-last-message", responsePath,
	)
	if turn.ThreadID != "" {
		args = append(args, turn.ThreadID)
	}
	args = append(args, "-")

	cmd := exec.CommandContext(ctx, a.bin, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 2 * time.Second
	cmd.Dir = turn.CWD
	cmd.Stdin = strings.NewReader(turn.Prompt)
	cmd.Stderr = a.progress
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return agentReply{}, err
	}
	if err := cmd.Start(); err != nil {
		return agentReply{}, fmt.Errorf("start %s: %w", a.bin, err)
	}

	threadID := turn.ThreadID
	sawThreadStarted := false
	var streamLines []string
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		streamLines = append(streamLines, line)
		var event struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
		}
		if json.Unmarshal([]byte(line), &event) == nil && event.Type == "thread.started" && event.ThreadID != "" {
			sawThreadStarted = true
			if threadID != "" && threadID != event.ThreadID {
				_ = cmd.Cancel()
				_ = cmd.Wait()
				return agentReply{}, fmt.Errorf("Codex resumed unexpected thread %s instead of %s", event.ThreadID, threadID)
			}
			threadID = event.ThreadID
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		_ = cmd.Cancel()
		_ = cmd.Wait()
		return agentReply{}, fmt.Errorf("read Codex JSON stream: %w", scanErr)
	}
	if err := cmd.Wait(); err != nil {
		detail := strings.Join(streamLines, "\n")
		if len(detail) > 4096 {
			detail = detail[len(detail)-4096:]
		}
		if detail != "" {
			return agentReply{}, fmt.Errorf("Codex turn failed: %w: %s", err, detail)
		}
		return agentReply{}, fmt.Errorf("Codex turn failed: %w", err)
	}
	if !sawThreadStarted || threadID == "" {
		return agentReply{}, errors.New("Codex JSON stream did not include thread.started")
	}
	data, err := os.ReadFile(responsePath)
	if err != nil {
		return agentReply{}, fmt.Errorf("read Codex final response: %w", err)
	}
	var structured codexStructuredReply
	if err := json.Unmarshal(data, &structured); err != nil {
		return agentReply{}, fmt.Errorf("decode Codex structured response: %w", err)
	}
	structured.Answer = strings.TrimSpace(structured.Answer)
	structured.Command = strings.TrimSpace(structured.Command)
	switch structured.Kind {
	case "answer":
		if structured.Answer == "" || structured.Command != "" {
			return agentReply{}, errors.New("Codex returned an invalid answer response")
		}
		return agentReply{ThreadID: threadID, Answer: structured.Answer}, nil
	case "shell":
		if structured.Command == "" || structured.Answer != "" {
			return agentReply{}, errors.New("Codex returned an invalid shell response")
		}
		actionID, err := newID("agent-")
		if err != nil {
			return agentReply{}, err
		}
		return agentReply{ThreadID: threadID, Action: &sharedAction{ID: actionID, Command: structured.Command}}, nil
	default:
		return agentReply{}, fmt.Errorf("Codex returned unknown response kind %q", structured.Kind)
	}
}

func initialAgentPrompt(userPrompt, contextText string) string {
	var prompt strings.Builder
	prompt.WriteString(`You are running as the agent inside Harnesh, a persistent interactive shell.

Harnesh and the user share one live PTY-backed shell. Your built-in shell tools are available for isolated repository work and background processing. Do not use them for work that must observe or change the live shell's cwd, exported or shell-local variables, aliases, functions, jobs, history, terminal state, or visible foreground interaction. For that work, return kind "shell" with exactly one command. Harnesh will run it visibly in the live shell and resume this same thread with its result.

Return kind "answer" when no live-shell command is needed. Put the user-facing response in answer and leave command empty. For kind "shell", put the command in command and leave answer empty. Do not claim a shared-shell command ran until Harnesh returns its result.`)
	if contextText != "" {
		prompt.WriteString("\n\n")
		prompt.WriteString(contextText)
	}
	prompt.WriteString("\n\n<user-request>\n")
	prompt.WriteString(userPrompt)
	prompt.WriteString("\n</user-request>")
	return prompt.String()
}
