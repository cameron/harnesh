package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type scriptedAdapter struct {
	mu      sync.Mutex
	replies []agentReply
	err     error
	turns   []agentTurn
}

func (a *scriptedAdapter) Name() string { return "agent" }

func (a *scriptedAdapter) Run(_ context.Context, turn agentTurn) (agentReply, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.turns = append(a.turns, turn)
	if a.err != nil {
		return agentReply{}, a.err
	}
	if len(a.replies) == 0 {
		return agentReply{}, errors.New("no scripted reply")
	}
	reply := a.replies[0]
	a.replies = a.replies[1:]
	return reply, nil
}

func (a *scriptedAdapter) recordedTurns() []agentTurn {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]agentTurn(nil), a.turns...)
}

func TestControllerContinuesSameThreadAfterSharedShellAction(t *testing.T) {
	j := testJournal(t)
	adapter := &scriptedAdapter{replies: []agentReply{
		{ThreadID: "pi:native-session", Action: &sharedAction{ID: "agent-action", Command: "cd /tmp && printf 'shared output\\n'"}},
		{ThreadID: "pi:native-session", Answer: "The live shell is now in /tmp."},
	}}
	var output bytes.Buffer
	controller := newAgentController(adapter, j, &output)
	first := controller.handle(context.Background(), promptRequest{Type: "prompt", Prompt: "go to tmp", CWD: "/tmp"})
	if first.Error != "" || first.ActionID != "agent-action" || first.Command == "" || !controller.active() {
		t.Fatalf("unexpected first response: %#v", first)
	}
	recordTestEvent(t, j, first.ActionID, "agent", first.Command, "/tmp", 0, []byte("shared output\n"))
	second := controller.handle(context.Background(), promptRequest{Type: "result", ActionID: first.ActionID, CWD: "/tmp"})
	if second.Error != "" || second.ActionID != "" || controller.active() {
		t.Fatalf("unexpected final response: %#v", second)
	}
	if !strings.Contains(output.String(), "live shell is now in /tmp") {
		t.Fatalf("answer was not displayed: %q", output.String())
	}
	turns := adapter.recordedTurns()
	if len(turns) != 2 || turns[0].ThreadID != "" || turns[1].ThreadID != "pi:native-session" {
		t.Fatalf("thread continuity failed: %#v", turns)
	}
	for _, required := range []string{"shared output", "exit_code: 0", "cd /tmp"} {
		if !strings.Contains(turns[1].Prompt, required) {
			t.Fatalf("action result prompt lacks %q:\n%s", required, turns[1].Prompt)
		}
	}
}

func TestControllerDeliversDirectShellEventsOnceAfterSuccessfulTurn(t *testing.T) {
	j := testJournal(t)
	recordTestEvent(t, j, "direct-export", "user", "export PROJECT=demo", "/tmp", 0, nil)
	adapter := &scriptedAdapter{replies: []agentReply{
		{ThreadID: "codex:thread", Answer: "first"},
		{ThreadID: "codex:thread", Answer: "second"},
	}}
	controller := newAgentController(adapter, j, &bytes.Buffer{})
	first := controller.handle(context.Background(), promptRequest{Type: "prompt", Prompt: "first question", CWD: "/tmp"})
	second := controller.handle(context.Background(), promptRequest{Type: "prompt", Prompt: "second question", CWD: "/tmp"})
	if first.Error != "" || second.Error != "" {
		t.Fatalf("turn failed: %#v %#v", first, second)
	}
	turns := adapter.recordedTurns()
	if !strings.Contains(turns[0].Prompt, "export PROJECT=demo") {
		t.Fatalf("first turn missed direct event:\n%s", turns[0].Prompt)
	}
	if strings.Contains(turns[1].Prompt, "export PROJECT=demo") {
		t.Fatalf("direct event was delivered twice:\n%s", turns[1].Prompt)
	}
	if j.adapter("agent").Cursor != 1 {
		t.Fatalf("agent cursor = %d", j.adapter("agent").Cursor)
	}
}

func TestControllerDoesNotAdvanceCursorAfterAgentFailure(t *testing.T) {
	j := testJournal(t)
	recordTestEvent(t, j, "direct-failure", "user", "false", "/tmp", 1, nil)
	adapter := &scriptedAdapter{err: errors.New("native thread is missing")}
	controller := newAgentController(adapter, j, &bytes.Buffer{})
	response := controller.handle(context.Background(), promptRequest{Type: "prompt", Prompt: "explain", CWD: "/tmp"})
	if response.Error == "" || j.adapter("agent").Cursor != 0 || controller.active() {
		t.Fatalf("failure state = response %#v cursor %d active %v", response, j.adapter("agent").Cursor, controller.active())
	}
}

func TestControllerEnforcesSharedActionLimit(t *testing.T) {
	j := testJournal(t)
	adapter := &scriptedAdapter{replies: []agentReply{
		{ThreadID: "claude:thread", Action: &sharedAction{ID: "action-one", Command: "pwd"}},
		{ThreadID: "claude:thread", Action: &sharedAction{ID: "action-two", Command: "pwd"}},
	}}
	controller := newAgentController(adapter, j, &bytes.Buffer{})
	controller.limit = 1
	first := controller.handle(context.Background(), promptRequest{Type: "prompt", Prompt: "loop", CWD: "/tmp"})
	recordTestEvent(t, j, first.ActionID, "agent", first.Command, "/tmp", 0, []byte("/tmp\n"))
	second := controller.handle(context.Background(), promptRequest{Type: "result", ActionID: first.ActionID})
	if second.Error == "" || !strings.Contains(second.Error, "action limit") || controller.active() {
		t.Fatalf("action limit response = %#v", second)
	}
}

type blockingAdapter struct {
	started chan struct{}
}

func (a *blockingAdapter) Name() string { return "agent" }

func (a *blockingAdapter) Run(ctx context.Context, _ agentTurn) (agentReply, error) {
	close(a.started)
	<-ctx.Done()
	return agentReply{}, ctx.Err()
}

func TestControllerCancellationStopsActiveAgentTurn(t *testing.T) {
	j := testJournal(t)
	adapter := &blockingAdapter{started: make(chan struct{})}
	controller := newAgentController(adapter, j, &bytes.Buffer{})
	done := make(chan promptResponse, 1)
	go func() {
		done <- controller.handle(context.Background(), promptRequest{Type: "prompt", Prompt: "wait", CWD: "/tmp"})
	}()
	select {
	case <-adapter.started:
	case <-time.After(2 * time.Second):
		t.Fatal("agent turn did not start")
	}
	controller.cancel()
	select {
	case response := <-done:
		if response.Error == "" || controller.active() {
			t.Fatalf("cancellation response = %#v", response)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled agent turn did not stop")
	}
}
