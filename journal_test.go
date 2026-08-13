package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testJournal(t *testing.T) *journal {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	j, err := newSession(t.TempDir(), "/bin/bash")
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func recordTestEvent(t *testing.T, j *journal, id, origin, command, cwd string, status int, output []byte) shellEvent {
	t.Helper()
	if err := j.begin(shellMarker{Type: "start", ID: id, Origin: origin, Command: command, CWD: cwd, Timestamp: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	j.appendOutput(output)
	if err := j.end(shellMarker{Type: "end", ID: id, CWD: cwd, ExitCode: status, Timestamp: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	event, err := j.waitEvent(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func TestJournalStoresPrivateStructuredEventAndExactOutput(t *testing.T) {
	j := testJournal(t)
	output := []byte("first line\nsecond line\n")
	event := recordTestEvent(t, j, "user-one", "user", "printf test", "/tmp", 7, output)
	if event.Sequence != 1 || event.ExitCode != 7 || event.CWD != "/tmp" || event.FinalCWD != "/tmp" {
		t.Fatalf("unexpected event: %#v", event)
	}
	stored, err := j.output(event)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, output) {
		t.Fatalf("stored output = %q", stored)
	}
	for _, path := range []string{j.dir, j.blobsDir} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %o", path, info.Mode().Perm())
		}
	}
	for _, path := range []string{j.metaPath, j.eventsPath, filepath.Join(j.dir, event.OutputRef)} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o", path, info.Mode().Perm())
		}
	}
	var copied bytes.Buffer
	if err := copyEventOutput(&copied, j, event.ID); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(copied.Bytes(), output) {
		t.Fatalf("history output = %q", copied.Bytes())
	}
}

func TestJournalProjectsGenerousHeadAndTail(t *testing.T) {
	j := testJournal(t)
	head := bytes.Repeat([]byte("H"), outputHeadLimit)
	middle := bytes.Repeat([]byte("M"), 8192)
	tail := bytes.Repeat([]byte("T"), outputTailLimit)
	event := recordTestEvent(t, j, "large-output", "user", "large", "/tmp", 0, append(append(head, middle...), tail...))
	projection, err := j.projectEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(projection, strings.Repeat("H", 1024)) < outputHeadLimit/1024 {
		t.Fatal("projection did not retain the 32 KiB head")
	}
	if strings.Count(projection, strings.Repeat("T", 1024)) < outputTailLimit/1024 {
		t.Fatal("projection did not retain the 32 KiB tail")
	}
	if !strings.Contains(projection, "8192 bytes omitted") || !strings.Contains(projection, "harnesh history output large-output") {
		t.Fatalf("projection lacks omission metadata or output reference")
	}
}

func TestJournalBatchesDirectEventsExactlyOncePerAdapter(t *testing.T) {
	j := testJournal(t)
	recordTestEvent(t, j, "direct-one", "user", "export VALUE=one", "/tmp", 0, []byte(""))
	recordTestEvent(t, j, "agent-one", "agent", "pwd", "/tmp", 0, []byte("/tmp\n"))
	recordTestEvent(t, j, "prompt-one", "prompt", "why?", "/tmp", 127, nil)

	batch, err := j.buildBatch("agent")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(batch.Text, "export VALUE=one") || strings.Contains(batch.Text, "command: pwd") || strings.Contains(batch.Text, "command: why?") {
		t.Fatalf("incorrect context batch:\n%s", batch.Text)
	}
	if batch.ID == "" || batch.Through != 3 {
		t.Fatalf("unexpected batch metadata: %#v", batch)
	}
	if err := j.updateAdapter("agent", "pi:thread", batch.Through); err != nil {
		t.Fatal(err)
	}
	again, err := j.buildBatch("agent")
	if err != nil {
		t.Fatal(err)
	}
	if again.Text != "" || again.Through != 3 {
		t.Fatalf("delivered events repeated: %#v", again)
	}
	other, err := j.buildBatch("claude")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(other.Text, "export VALUE=one") || j.adapter("claude").Cursor != 0 {
		t.Fatalf("adapter cursor was not independent: %#v", other)
	}
}

func TestJournalMigratesLegacyCodexSessionToAgentBridge(t *testing.T) {
	j := testJournal(t)
	recordTestEvent(t, j, "legacy-event", "user", "pwd", "/tmp", 0, []byte("/tmp\n"))
	j.meta.Adapters["codex"] = adapterState{ThreadID: "legacy-thread", Cursor: 1}
	if err := j.saveMetaLocked(); err != nil {
		t.Fatal(err)
	}
	if err := j.migrateLegacyCodexAdapter(); err != nil {
		t.Fatal(err)
	}
	state := j.adapter("agent")
	if state.ThreadID != "codex:legacy-thread" || state.Cursor != 1 {
		t.Fatalf("migrated adapter state = %#v", state)
	}
	reopened, err := openSession(j.sessionID())
	if err != nil {
		t.Fatal(err)
	}
	if reopened.adapter("agent") != state {
		t.Fatalf("persisted adapter state = %#v", reopened.adapter("agent"))
	}
}

func TestJournalMigrationPreservesExistingAgentSession(t *testing.T) {
	j := testJournal(t)
	j.meta.Adapters["codex"] = adapterState{ThreadID: "legacy-thread", Cursor: 2}
	j.meta.Adapters["agent"] = adapterState{ThreadID: "pi:active-session", Cursor: 5}
	if err := j.migrateLegacyCodexAdapter(); err != nil {
		t.Fatal(err)
	}
	state := j.adapter("agent")
	if state.ThreadID != "pi:active-session" || state.Cursor != 5 {
		t.Fatalf("existing agent state changed: %#v", state)
	}
}

func TestJournalBatchStopsBeforeLimitAndKeepsStableID(t *testing.T) {
	j := testJournal(t)
	output := bytes.Repeat([]byte("x"), outputHeadLimit+outputTailLimit)
	for index := 0; index < 6; index++ {
		recordTestEvent(t, j, "batch-"+string(rune('a'+index)), "user", "command", "/tmp", 0, output)
	}
	first, err := j.buildBatch("agent")
	if err != nil {
		t.Fatal(err)
	}
	second, err := j.buildBatch("agent")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.Through != second.Through {
		t.Fatalf("retry batch changed: %#v then %#v", first, second)
	}
	if first.Through >= 6 || len(first.Text) > contextBatchLimit+4096 {
		t.Fatalf("batch limit was not applied: through=%d bytes=%d", first.Through, len(first.Text))
	}
}

func TestJournalRejectsCorruptMetadataAndEvents(t *testing.T) {
	j := testJournal(t)
	if err := os.WriteFile(j.metaPath, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openSession(j.sessionID()); err == nil {
		t.Fatal("corrupt metadata opened successfully")
	}

	j = testJournal(t)
	bad, _ := json.Marshal(shellEvent{Version: 99, ID: "bad", Sequence: 1})
	if err := os.WriteFile(j.eventsPath, append(bad, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := j.events(); err == nil {
		t.Fatal("corrupt event journal was accepted")
	}
}

func TestResumeUsesRecordedCWDWithoutReplayingState(t *testing.T) {
	j := testJournal(t)
	recorded := t.TempDir()
	if err := j.updateCWD(recorded); err != nil {
		t.Fatal(err)
	}
	reopened, err := openSession(j.sessionID())
	if err != nil {
		t.Fatal(err)
	}
	warnings, err := os.CreateTemp(t.TempDir(), "warnings")
	if err != nil {
		t.Fatal(err)
	}
	if got := selectResumeCWD(reopened.lastCWD(), "/fallback", warnings); got != recorded {
		t.Fatalf("resume cwd = %s", got)
	}
	if err := os.Remove(recorded); err != nil {
		t.Fatal(err)
	}
	fallback := t.TempDir()
	if got := selectResumeCWD(reopened.lastCWD(), fallback, warnings); got != fallback {
		t.Fatalf("missing cwd fallback = %s", got)
	}
	if len(reopened.stack) != 0 {
		t.Fatal("resume replayed shell commands")
	}
}

func TestOpenSessionRepairsMetadataAfterCommittedJournalEvent(t *testing.T) {
	j := testJournal(t)
	finalCWD := t.TempDir()
	event := recordTestEvent(t, j, "committed-event", "user", "cd repaired", finalCWD, 0, nil)
	j.meta.NextSeq = event.Sequence
	j.meta.LastCWD = "/stale-cwd"
	j.meta.UpdatedAt = event.StartedAt.Add(-time.Second)
	if err := j.saveMetaLocked(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openSession(j.sessionID())
	if err != nil {
		t.Fatal(err)
	}
	if reopened.meta.NextSeq != event.Sequence+1 || reopened.lastCWD() != finalCWD {
		t.Fatalf("repaired metadata = %#v", reopened.meta)
	}
}

func TestOpenSessionRejectsModifiedOutputBlob(t *testing.T) {
	j := testJournal(t)
	event := recordTestEvent(t, j, "exact-blob", "user", "printf exact", "/tmp", 0, []byte("exact\n"))
	path := filepath.Join(j.dir, event.OutputRef)
	if err := os.WriteFile(path, []byte("modified output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openSession(j.sessionID()); err == nil || !strings.Contains(err.Error(), "invalid output blob") {
		t.Fatalf("modified output blob error = %v", err)
	}
}

func TestExpectedActionCanStartAfterResultWaitBegins(t *testing.T) {
	j := testJournal(t)
	if err := j.expect("delayed-action"); err != nil {
		t.Fatal(err)
	}
	result := make(chan shellEvent, 1)
	errs := make(chan error, 1)
	go func() {
		event, err := j.waitEvent(context.Background(), "delayed-action")
		if err != nil {
			errs <- err
			return
		}
		result <- event
	}()
	recordTestEvent(t, j, "delayed-action", "agent", "pwd", "/tmp", 0, []byte("/tmp\n"))
	select {
	case err := <-errs:
		t.Fatal(err)
	case event := <-result:
		if event.ID != "delayed-action" || event.OutputBytes == 0 {
			t.Fatalf("delayed event = %#v", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected action result did not wake")
	}
}
