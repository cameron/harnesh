package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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

type agentBridge struct {
	bin      string
	progress io.Writer
}

type bridgeResponse struct {
	Harness   string `json:"harness"`
	SessionID string `json:"session_id"`
	Kind      string `json:"kind"`
	Answer    string `json:"answer"`
	Command   string `json:"command"`
}

func newAgentBridge() *agentBridge {
	bin := os.Getenv("HARNESH_AGENT_BIN")
	if bin == "" {
		bin = "agent"
	}
	return &agentBridge{bin: bin, progress: os.Stderr}
}

func (a *agentBridge) Name() string { return "agent" }

func (a *agentBridge) Run(ctx context.Context, turn agentTurn) (agentReply, error) {
	if turn.CWD == "" || turn.Prompt == "" {
		return agentReply{}, errors.New("agent turn requires cwd and prompt")
	}
	args := []string{"--here", "--harnesh-turn"}
	if turn.ThreadID != "" {
		args = append(args, "--session", turn.ThreadID)
	}
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
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stdout.String())
		if len(detail) > 4096 {
			detail = detail[len(detail)-4096:]
		}
		if detail != "" {
			return agentReply{}, fmt.Errorf("agent turn failed: %w: %s", err, detail)
		}
		return agentReply{}, fmt.Errorf("agent turn failed: %w", err)
	}

	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	var response bridgeResponse
	if err := decoder.Decode(&response); err != nil {
		return agentReply{}, fmt.Errorf("decode agent bridge response: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return agentReply{}, errors.New("agent bridge returned more than one JSON response")
	}
	response.Answer = strings.TrimSpace(response.Answer)
	response.Command = strings.TrimSpace(response.Command)
	if !validAgentSessionID(response.SessionID, response.Harness) {
		return agentReply{}, errors.New("agent bridge returned an invalid session ID")
	}
	if turn.ThreadID != "" && response.SessionID != turn.ThreadID {
		return agentReply{}, fmt.Errorf("agent resumed %s instead of %s", response.SessionID, turn.ThreadID)
	}
	switch response.Kind {
	case "answer":
		if response.Answer == "" || response.Command != "" {
			return agentReply{}, errors.New("agent bridge returned an invalid answer response")
		}
		return agentReply{ThreadID: response.SessionID, Answer: response.Answer}, nil
	case "shell":
		if response.Command == "" || response.Answer != "" {
			return agentReply{}, errors.New("agent bridge returned an invalid shell response")
		}
		actionID, err := newID("agent-")
		if err != nil {
			return agentReply{}, err
		}
		return agentReply{
			ThreadID: response.SessionID,
			Action:   &sharedAction{ID: actionID, Command: response.Command},
		}, nil
	default:
		return agentReply{}, fmt.Errorf("agent bridge returned unknown response kind %q", response.Kind)
	}
}

func validAgentSessionID(sessionID, harness string) bool {
	selected, native, ok := strings.Cut(sessionID, ":")
	if !ok || selected != harness || !validID(native) {
		return false
	}
	switch selected {
	case "codex", "claude", "pi":
		return true
	default:
		return false
	}
}

func initialAgentPrompt(userPrompt, contextText string) string {
	var prompt strings.Builder
	prompt.WriteString(`You are running as the agent inside Harnesh, a persistent interactive shell.

Harnesh and the user share one live PTY-backed shell. Your built-in shell tools are available for isolated repository work and background processing. Do not use them for work that must observe or change the live shell's cwd, exported or shell-local variables, aliases, functions, jobs, history, terminal state, or visible foreground interaction. For that work, return kind "shell" with exactly one command. Harnesh will run it visibly in the live shell and resume this same native agent session with its result.

Return kind "answer" when no live-shell command is needed. Put the user-facing response in answer and leave command empty. For kind "shell", put the command in command and leave answer empty. Do not claim a shared-shell command ran until Harnesh returns its result. Return only the JSON object with the kind, answer, and command fields.`)
	if contextText != "" {
		prompt.WriteString("\n\n")
		prompt.WriteString(contextText)
	}
	prompt.WriteString("\n\n<user-request>\n")
	prompt.WriteString(userPrompt)
	prompt.WriteString("\n</user-request>")
	return prompt.String()
}
