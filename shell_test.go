package main

import (
	"bytes"
	"encoding/base64"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

func TestMarkerStreamParsesFragmentedNestedEvents(t *testing.T) {
	j := testJournal(t)
	var terminal bytes.Buffer
	var logs bytes.Buffer
	stream := &markerStream{journal: j, supported: true, output: &terminal, log: &logs}
	startPrompt := markerBytes(t, "start", "prompt-event", "user", ", inspect", "/tmp")
	markPrompt := markerBytes(t, "prompt", "prompt-event")
	startAction := markerBytes(t, "start", "agent-event", "agent", "printf nested", "/tmp")
	endAction := markerBytes(t, "end", "agent-event", "0", "/tmp")
	endPrompt := markerBytes(t, "end", "prompt-event", "0", "/tmp")
	payload := bytes.Join([][]byte{
		[]byte("before marker\n"), startPrompt, markPrompt, []byte("prompt output\n"), startAction,
		[]byte("nested output\n"), endAction, []byte("after action\n"), endPrompt,
	}, nil)
	for offset := 0; offset < len(payload); {
		next := offset + 7
		if next > len(payload) {
			next = len(payload)
		}
		stream.write(payload[offset:next])
		offset = next
	}
	stream.flush()
	if logs.Len() != 0 {
		t.Fatalf("marker parser errors: %s", logs.String())
	}
	if strings.Contains(terminal.String(), "HARNESH:") || !strings.Contains(terminal.String(), "nested output") {
		t.Fatalf("terminal projection is wrong: %q", terminal.String())
	}
	events, err := j.events()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].ID != "agent-event" || events[1].ID != "prompt-event" || events[1].Origin != "prompt" {
		t.Fatalf("nested completion order = %#v", events)
	}
	actionOutput, _ := j.output(events[0])
	promptOutput, _ := j.output(events[1])
	if string(actionOutput) != "nested output\n" || !strings.Contains(string(promptOutput), "prompt output") || !strings.Contains(string(promptOutput), "after action") {
		t.Fatalf("nested outputs action=%q prompt=%q", actionOutput, promptOutput)
	}
}

func TestPromptMarkerExcludesCommandFromAgentContext(t *testing.T) {
	j := testJournal(t)
	if err := j.begin(shellMarker{Type: "start", ID: "prompt", Origin: "user", Command: "please explain", CWD: "/tmp"}); err != nil {
		t.Fatal(err)
	}
	if err := j.markPrompt("prompt"); err != nil {
		t.Fatal(err)
	}
	if err := j.end(shellMarker{Type: "end", ID: "prompt", CWD: "/tmp", ExitCode: 127}); err != nil {
		t.Fatal(err)
	}
	events, err := j.events()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Origin != "prompt" {
		t.Fatalf("prompt event = %#v", events)
	}
	batch, err := j.buildBatch("agent")
	if err != nil {
		t.Fatal(err)
	}
	if batch.Text != "" {
		t.Fatalf("agent prompt leaked into shell context: %s", batch.Text)
	}
}

func TestBashAndFishHooksReportCommandLifecycle(t *testing.T) {
	for _, kind := range []shellKind{shellBash, shellFish} {
		t.Run(string(kind), func(t *testing.T) {
			if _, err := exec.LookPath(string(kind)); err != nil {
				t.Skipf("%s is unavailable", kind)
			}
			dir := t.TempDir()
			t.Setenv("HOME", dir)
			logPath := filepath.Join(dir, "markers.log")
			fake := filepath.Join(dir, "harnesh-marker")
			script := "printf '%s\\t' \"$@\" >> " + shellQuote(logPath) + "\nprintf '\\n' >> " + shellQuote(logPath) + "\n"
			writeBashTestScript(t, fake, script)
			var initPath string
			var args []string
			var err error
			if kind == shellBash {
				initPath, err = writeBashInitFile(fake)
				args = []string{"--noprofile", "--init-file", initPath, "-i"}
			} else {
				initPath, err = writeFishInitFile(fake)
				args = []string{"--no-config", "-C", "source " + fishQuote(initPath)}
			}
			if err != nil {
				t.Fatal(err)
			}
			defer os.Remove(initPath)
			cmd := exec.Command(string(kind), args...)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "HARNESH_BIN="+fake, "HARNESH_PROMPT_SOCKET="+filepath.Join(dir, "unused.sock"))
			ptmx, err := pty.Start(cmd)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.WriteString(ptmx, "printf 'hook-output\\n'\nfalse\nexit\n")
			done := make(chan error, 1)
			go func() { _, readErr := io.Copy(io.Discard, ptmx); done <- readErr }()
			_ = cmd.Wait()
			_ = ptmx.Close()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("shell PTY did not close")
			}
			log, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			text := string(log)
			for _, required := range []string{"__marker\tstart\t", "user\tprintf 'hook-output", "__marker\tend\t", "\t1\t"} {
				if !strings.Contains(text, required) {
					t.Fatalf("%s lifecycle log lacks %q:\n%s", kind, required, text)
				}
			}
		})
	}
}

func TestFishAgentActionRunsAtTopLevelAndEntersHistory(t *testing.T) {
	if _, err := exec.LookPath("fish"); err != nil {
		t.Skip("fish is unavailable")
	}
	dir := t.TempDir()
	actionDir := filepath.Join(dir, "action-cwd")
	if err := os.Mkdir(actionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	action := "set HARNESH_FISH_LOCAL local; cd " + fishQuote(actionDir) + "; printf 'fish-action-done\\n'"
	encoded := base64.StdEncoding.EncodeToString([]byte(action))
	fake := filepath.Join(dir, "harnesh-fake")
	script := "case \"${1-}\" in\n" +
		"  __agent-prompt) printf 'fish-action\\t%s\\n' " + shellQuote(encoded) + " ;;\n" +
		"  __agent-result|__marker) : ;;\n" +
		"esac\n"
	writeBashTestScript(t, fake, script)
	initPath, err := writeFishInitFile(fake)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(initPath)
	pending := filepath.Join(dir, "pending-prompt")
	if err := os.WriteFile(pending, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("fish", "--no-config", "-C", "source "+fishQuote(initPath))
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"HOME="+dir,
		"HARNESH_BIN="+fake,
		"HARNESH_PENDING_PROMPT_FILE="+pending,
	)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	check := "printf 'fish-state:%s:%s\\n' \"$HARNESH_FISH_LOCAL\" \"$PWD\"; " +
		"if contains -- " + fishQuote(action) + " (history); echo fish-history-ok; end"
	_, _ = io.WriteString(ptmx, ", configure fish\n"+check+"\nexit\n")
	var output bytes.Buffer
	done := make(chan error, 1)
	go func() { _, readErr := io.Copy(&output, ptmx); done <- readErr }()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("fish exited with %v; output:\n%s", err, output.String())
	}
	_ = ptmx.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("fish PTY did not close")
	}
	text := output.String()
	if !strings.Contains(text, "fish-action-done") || !strings.Contains(text, "fish-state:local:"+actionDir) || !strings.Contains(text, "fish-history-ok") {
		t.Fatalf("Fish did not preserve the agent action state and history:\n%s", text)
	}
}

func markerBytes(t *testing.T, args ...string) []byte {
	t.Helper()
	var output bytes.Buffer
	if err := writeMarker(args, &output); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}
