package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

type promptRequest struct {
	Type     string `json:"type"`
	Prompt   string `json:"prompt,omitempty"`
	CWD      string `json:"cwd,omitempty"`
	ActionID string `json:"action_id,omitempty"`
}

type promptResponse struct {
	ActionID string `json:"action_id,omitempty"`
	Command  string `json:"command,omitempty"`
	Error    string `json:"error,omitempty"`
}

type agentController struct {
	adapter agentAdapter
	journal *journal
	output  io.Writer
	limit   int

	runMu sync.Mutex
	mu    sync.Mutex
	live  atomic.Bool

	canceled   bool
	cancelTurn context.CancelFunc
	pending    *sharedAction
	actionRuns int
}

func newAgentController(adapter agentAdapter, journal *journal, output io.Writer) *agentController {
	limit := 32
	if configured := os.Getenv("HARNESH_ACTION_LIMIT"); configured != "" {
		if parsed, err := strconv.Atoi(configured); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	return &agentController{adapter: adapter, journal: journal, output: output, limit: limit}
}

func (c *agentController) active() bool { return c.live.Load() }

func (c *agentController) cancel() {
	c.mu.Lock()
	if !c.live.Load() {
		c.mu.Unlock()
		return
	}
	c.canceled = true
	c.pending = nil
	cancelTurn := c.cancelTurn
	c.live.Store(false)
	c.mu.Unlock()
	if cancelTurn != nil {
		cancelTurn()
	}
}

func (c *agentController) handle(ctx context.Context, request promptRequest) promptResponse {
	c.runMu.Lock()
	defer c.runMu.Unlock()
	switch request.Type {
	case "prompt":
		return c.handlePrompt(ctx, request)
	case "result":
		return c.handleResult(ctx, request)
	default:
		return promptResponse{Error: fmt.Sprintf("unknown prompt request type %q", request.Type)}
	}
}

func (c *agentController) handlePrompt(ctx context.Context, request promptRequest) promptResponse {
	prompt := strings.TrimSpace(request.Prompt)
	if prompt == "" {
		return promptResponse{Error: "empty agent prompt"}
	}
	c.mu.Lock()
	if c.live.Load() {
		c.mu.Unlock()
		return promptResponse{Error: "another agent turn owns the shell"}
	}
	c.canceled = false
	c.pending = nil
	c.actionRuns = 0
	c.live.Store(true)
	c.mu.Unlock()

	if err := c.journal.updateCWD(request.CWD); err != nil {
		return c.fail(err)
	}
	batch, err := c.journal.buildBatch(c.adapter.Name())
	if err != nil {
		return c.fail(err)
	}
	state := c.journal.adapter(c.adapter.Name())
	prompt = initialAgentPrompt(prompt, batch.Text)
	reply, err := c.runTurn(ctx, agentTurn{CWD: c.journal.lastCWD(), ThreadID: state.ThreadID, Prompt: prompt})
	if err != nil {
		return c.fail(err)
	}
	if err := c.journal.updateAdapter(c.adapter.Name(), reply.ThreadID, batch.Through); err != nil {
		return c.fail(err)
	}
	return c.acceptReply(reply)
}

func (c *agentController) handleResult(ctx context.Context, request promptRequest) promptResponse {
	c.mu.Lock()
	if c.canceled || !c.live.Load() {
		c.mu.Unlock()
		return promptResponse{Error: "agent action loop was canceled"}
	}
	pending := c.pending
	c.mu.Unlock()
	if pending == nil || request.ActionID != pending.ID {
		return c.fail(fmt.Errorf("unexpected shell action result %q", request.ActionID))
	}
	event, err := c.journal.waitEvent(ctx, request.ActionID)
	if err != nil {
		return c.fail(err)
	}
	resultText, err := c.journal.actionResultText(event)
	if err != nil {
		return c.fail(err)
	}
	batch, err := c.journal.buildBatch(c.adapter.Name())
	if err != nil {
		return c.fail(err)
	}
	if batch.Text != "" {
		resultText += "\n\n" + batch.Text
	}
	state := c.journal.adapter(c.adapter.Name())
	if state.ThreadID == "" {
		return c.fail(errors.New("agent session ID is missing from the active Harnesh session"))
	}
	reply, err := c.runTurn(ctx, agentTurn{CWD: c.journal.lastCWD(), ThreadID: state.ThreadID, Prompt: resultText})
	if err != nil {
		return c.fail(err)
	}
	if err := c.journal.updateAdapter(c.adapter.Name(), reply.ThreadID, batch.Through); err != nil {
		return c.fail(err)
	}
	return c.acceptReply(reply)
}

func (c *agentController) runTurn(parent context.Context, turn agentTurn) (agentReply, error) {
	ctx, cancel := context.WithCancel(parent)
	c.mu.Lock()
	if c.canceled {
		c.mu.Unlock()
		cancel()
		return agentReply{}, context.Canceled
	}
	c.cancelTurn = cancel
	c.mu.Unlock()
	reply, err := c.adapter.Run(ctx, turn)
	c.mu.Lock()
	c.cancelTurn = nil
	canceled := c.canceled
	c.mu.Unlock()
	cancel()
	if canceled && err == nil {
		return agentReply{}, context.Canceled
	}
	return reply, err
}

func (c *agentController) acceptReply(reply agentReply) promptResponse {
	if reply.Action != nil {
		if err := c.journal.expect(reply.Action.ID); err != nil {
			return c.fail(err)
		}
		c.mu.Lock()
		if c.canceled {
			c.mu.Unlock()
			return c.fail(context.Canceled)
		}
		if c.actionRuns >= c.limit {
			c.mu.Unlock()
			return c.fail(fmt.Errorf("agent exceeded the %d shared-shell action limit", c.limit))
		}
		c.actionRuns++
		c.pending = reply.Action
		c.mu.Unlock()
		return promptResponse{ActionID: reply.Action.ID, Command: reply.Action.Command}
	}
	if reply.Answer == "" {
		return c.fail(errors.New("agent returned neither an answer nor a shared-shell action"))
	}
	fmt.Fprintf(c.output, "\r\n%s\r\n", reply.Answer)
	c.mu.Lock()
	c.pending = nil
	c.canceled = false
	c.live.Store(false)
	c.mu.Unlock()
	return promptResponse{}
}

func (c *agentController) fail(err error) promptResponse {
	if err == nil {
		err = errors.New("unknown agent error")
	}
	if !errors.Is(err, context.Canceled) {
		fmt.Fprintf(c.output, "\r\n[harnesh: %v]\r\n", err)
	}
	c.mu.Lock()
	c.pending = nil
	c.cancelTurn = nil
	c.live.Store(false)
	c.mu.Unlock()
	return promptResponse{Error: err.Error()}
}

func startPromptServer(controller *agentController) (string, func(), error) {
	dir, err := os.MkdirTemp("", "harnesh-prompt-")
	if err != nil {
		return "", func() {}, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", func() {}, err
	}
	socketPath := filepath.Join(dir, "prompt.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return "", func() {}, err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go handlePromptConnection(conn, controller)
		}
	}()
	cleanup := func() {
		_ = listener.Close()
		<-done
		_ = os.RemoveAll(dir)
	}
	return socketPath, cleanup, nil
}

func handlePromptConnection(conn net.Conn, controller *agentController) {
	defer conn.Close()
	var request promptRequest
	if err := json.NewDecoder(conn).Decode(&request); err != nil {
		_ = json.NewEncoder(conn).Encode(promptResponse{Error: err.Error()})
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	readDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, conn)
		cancel()
		close(readDone)
	}()
	response := controller.handle(ctx, request)
	_ = json.NewEncoder(conn).Encode(response)
}

func sendPromptRequest(socketPath string, request promptRequest) (promptResponse, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return promptResponse{}, err
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return promptResponse{}, err
	}
	var response promptResponse
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		return promptResponse{}, err
	}
	if response.Error != "" {
		return response, errors.New(response.Error)
	}
	return response, nil
}
