package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

const (
	markerPrefix = "\x1eHARNESH:"
	markerSuffix = "\x1f"
)

type shellKind string

const (
	shellBash        shellKind = "bash"
	shellFish        shellKind = "fish"
	shellUnsupported shellKind = "unsupported"
)

type markerStream struct {
	journal   *journal
	supported bool
	output    io.Writer
	log       io.Writer
	pending   []byte
}

func detectShellKind(shell string) shellKind {
	switch filepath.Base(shell) {
	case "bash":
		return shellBash
	case "fish":
		return shellFish
	default:
		return shellUnsupported
	}
}

func writeMarker(args []string, output io.Writer) error {
	if len(args) < 1 {
		return errors.New("marker type is required")
	}
	marker := shellMarker{Type: args[0], Timestamp: time.Now().UTC()}
	switch marker.Type {
	case "start":
		if len(args) != 5 {
			return errors.New("start marker requires ID, origin, command, and cwd")
		}
		marker.ID, marker.Origin, marker.Command, marker.CWD = args[1], args[2], args[3], args[4]
	case "prompt":
		if len(args) != 2 {
			return errors.New("prompt marker requires ID")
		}
		marker.ID = args[1]
	case "end":
		if len(args) != 4 {
			return errors.New("end marker requires ID, exit code, and cwd")
		}
		marker.ID, marker.CWD = args[1], args[3]
		exitCode, err := strconv.Atoi(args[2])
		if err != nil {
			return fmt.Errorf("invalid marker exit code: %w", err)
		}
		marker.ExitCode = exitCode
	default:
		return fmt.Errorf("unknown marker type %q", marker.Type)
	}
	data, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	encoded := base64.RawStdEncoding.EncodeToString(data)
	_, err = fmt.Fprintf(output, "%s%s%s", markerPrefix, encoded, markerSuffix)
	return err
}

func (s *markerStream) write(data []byte) {
	s.pending = append(s.pending, data...)
	prefix := []byte(markerPrefix)
	suffix := []byte(markerSuffix)
	for len(s.pending) > 0 {
		start := bytes.Index(s.pending, prefix)
		if start < 0 {
			keep := partialPrefixLength(s.pending, prefix)
			emitThrough := len(s.pending) - keep
			s.emit(s.pending[:emitThrough])
			s.pending = append([]byte(nil), s.pending[emitThrough:]...)
			return
		}
		if start > 0 {
			s.emit(s.pending[:start])
			s.pending = s.pending[start:]
		}
		end := bytes.Index(s.pending[len(prefix):], suffix)
		if end < 0 {
			return
		}
		end += len(prefix)
		encoded := s.pending[len(prefix):end]
		s.pending = append([]byte(nil), s.pending[end+len(suffix):]...)
		decoded, err := base64.RawStdEncoding.DecodeString(string(encoded))
		if err != nil {
			fmt.Fprintf(s.log, "harnesh: invalid shell marker: %v\n", err)
			continue
		}
		var marker shellMarker
		if err := json.Unmarshal(decoded, &marker); err != nil {
			fmt.Fprintf(s.log, "harnesh: invalid shell marker payload: %v\n", err)
			continue
		}
		switch marker.Type {
		case "start":
			if err := s.journal.begin(marker); err != nil {
				fmt.Fprintf(s.log, "harnesh: record shell command: %v\n", err)
			}
		case "prompt":
			if err := s.journal.markPrompt(marker.ID); err != nil {
				fmt.Fprintf(s.log, "harnesh: mark agent prompt: %v\n", err)
			}
		case "end":
			if err := s.journal.end(marker); err != nil {
				fmt.Fprintf(s.log, "harnesh: finish shell command: %v\n", err)
			}
		}
	}
}

func (s *markerStream) flush() {
	if len(s.pending) > 0 {
		s.emit(s.pending)
		s.pending = nil
	}
}

func (s *markerStream) emit(data []byte) {
	if len(data) == 0 {
		return
	}
	_, _ = s.output.Write(data)
	if s.supported {
		s.journal.appendOutput(data)
	} else {
		s.journal.appendRaw(data)
	}
}

func partialPrefixLength(data, prefix []byte) int {
	max := len(prefix) - 1
	if len(data) < max {
		max = len(data)
	}
	for size := max; size > 0; size-- {
		if bytes.Equal(data[len(data)-size:], prefix[:size]) {
			return size
		}
	}
	return 0
}

func runAgentPromptHelper(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: , <prompt>")
		return 2
	}
	socketPath := os.Getenv("HARNESH_PROMPT_SOCKET")
	if socketPath == "" {
		fmt.Fprintln(os.Stderr, "harnesh: HARNESH_PROMPT_SOCKET is not set")
		return 1
	}
	cwd, _ := os.Getwd()
	response, err := sendPromptRequest(socketPath, promptRequest{
		Type:   "prompt",
		Prompt: strings.TrimSpace(strings.Join(args, " ")),
		CWD:    cwd,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "harnesh: %v\n", err)
		return 1
	}
	writeActionResponse(response)
	return 0
}

func runAgentResultHelper(args []string) int {
	if len(args) != 1 || !validID(args[0]) {
		fmt.Fprintln(os.Stderr, "harnesh: action result requires one valid action ID")
		return 2
	}
	socketPath := os.Getenv("HARNESH_PROMPT_SOCKET")
	if socketPath == "" {
		fmt.Fprintln(os.Stderr, "harnesh: HARNESH_PROMPT_SOCKET is not set")
		return 1
	}
	cwd, _ := os.Getwd()
	response, err := sendPromptRequest(socketPath, promptRequest{Type: "result", ActionID: args[0], CWD: cwd})
	if err != nil {
		fmt.Fprintf(os.Stderr, "harnesh: %v\n", err)
		return 1
	}
	writeActionResponse(response)
	return 0
}

func writeActionResponse(response promptResponse) {
	if response.ActionID == "" {
		return
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(response.Command))
	fmt.Printf("%s\t%s\n", response.ActionID, encoded)
}

func runShell(j *journal, controller *agentController, cwd string) error {
	shell := os.Getenv("SHELL")
	if shell == "" {
		var err error
		shell, err = exec.LookPath("bash")
		if err != nil {
			return errors.New("SHELL is unset and bash is unavailable")
		}
	}
	kind := detectShellKind(shell)
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	promptSocket, cleanupSocket, err := startPromptServer(controller)
	if err != nil {
		return err
	}
	defer cleanupSocket()
	pendingPromptPath := filepath.Join(filepath.Dir(promptSocket), "pending-prompt")
	if err := writePrivateFile(pendingPromptPath, nil); err != nil {
		return err
	}
	args, cleanupInit, err := shellArguments(kind, executable)
	if err != nil {
		return err
	}
	defer cleanupInit()
	cmd := exec.Command(shell, args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(),
		"HARNESH=1",
		"HARNESH_BIN="+executable,
		"HARNESH_PROMPT_SOCKET="+promptSocket,
		"HARNESH_PENDING_PROMPT_FILE="+pendingPromptPath,
		"HARNESH_SESSION_ID="+j.sessionID(),
	)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return err
	}
	defer ptmx.Close()
	resize := make(chan os.Signal, 1)
	resizeDone := make(chan struct{})
	signal.Notify(resize, syscall.SIGWINCH)
	defer func() {
		signal.Stop(resize)
		close(resizeDone)
	}()
	go func() {
		for {
			select {
			case <-resize:
				_ = pty.InheritSize(os.Stdin, ptmx)
			case <-resizeDone:
				return
			}
		}
	}()
	resize <- syscall.SIGWINCH

	restore, err := makeRaw(int(os.Stdin.Fd()))
	if err != nil {
		_ = ptmx.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err
	}
	defer restore()

	stream := &markerStream{journal: j, supported: kind != shellUnsupported, output: os.Stdout, log: os.Stderr}
	outputDone := make(chan struct{})
	go func() {
		defer close(outputDone)
		buffer := make([]byte, 4096)
		for {
			count, readErr := ptmx.Read(buffer)
			if count > 0 {
				stream.write(buffer[:count])
			}
			if readErr != nil {
				stream.flush()
				return
			}
		}
	}()
	go copyInput(ptmx, controller)
	waitErr := cmd.Wait()
	_ = ptmx.Close()
	<-outputDone
	return waitErr
}

func copyInput(ptmx *os.File, controller *agentController) {
	buffer := make([]byte, 1024)
	for {
		count, err := os.Stdin.Read(buffer)
		if count > 0 {
			if controller.active() {
				for _, char := range buffer[:count] {
					if char == 3 {
						controller.cancel()
						_, _ = ptmx.Write([]byte{char})
					}
				}
			} else {
				_, _ = ptmx.Write(buffer[:count])
			}
		}
		if err != nil {
			return
		}
	}
}

func shellArguments(kind shellKind, executable string) ([]string, func(), error) {
	switch kind {
	case shellBash:
		path, err := writeBashInitFile(executable)
		if err != nil {
			return nil, func() {}, err
		}
		return []string{"--init-file", path, "-i"}, func() { _ = os.Remove(path) }, nil
	case shellFish:
		path, err := writeFishInitFile(executable)
		if err != nil {
			return nil, func() {}, err
		}
		return []string{"-C", "source " + fishQuote(path)}, func() { _ = os.Remove(path) }, nil
	default:
		return nil, func() {}, nil
	}
}

func writeBashInitFile(_ string) (string, error) {
	file, err := os.CreateTemp("", "harnesh-bashrc-*")
	if err != nil {
		return "", err
	}
	path := file.Name()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return "", err
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		fmt.Fprintf(file, "test -f %s && source %s\n", shellQuote(filepath.Join(home, ".bashrc")), shellQuote(filepath.Join(home, ".bashrc")))
	}
	script := `
__harnesh_previous_prompt_command="${PROMPT_COMMAND-}"
__harnesh_prompt_ready=1
__harnesh_event_id=

__harnesh_preexec() {
  local command_text="$1"
  [[ "${__harnesh_prompt_ready:-0}" == 1 ]] || return 0
  [[ "$command_text" != "__harnesh_postexec" ]] || return 0
  __harnesh_prompt_ready=0
  __harnesh_event_id="user-$$-${HISTCMD:-0}-$RANDOM"
  command "$HARNESH_BIN" __marker start "$__harnesh_event_id" user "$command_text" "$PWD"
}

__harnesh_postexec() {
  local command_status=$?
	local pending_prompt=
  if [[ -n "${__harnesh_event_id:-}" ]]; then
    command "$HARNESH_BIN" __marker end "$__harnesh_event_id" "$command_status" "$PWD"
    __harnesh_event_id=
  fi
	if [[ -s "$HARNESH_PENDING_PROMPT_FILE" ]]; then
		IFS= read -r -d '' pending_prompt < "$HARNESH_PENDING_PROMPT_FILE" || true
		: > "$HARNESH_PENDING_PROMPT_FILE"
		if [[ -n "$pending_prompt" ]]; then
			__harnesh_agent_prompt "$pending_prompt"
		fi
	fi
  __harnesh_prompt_ready=1
  if [[ -n "$__harnesh_previous_prompt_command" && "$__harnesh_previous_prompt_command" != "__harnesh_postexec" ]]; then
    builtin eval "$__harnesh_previous_prompt_command"
  fi
  return "$command_status"
}

__harnesh_agent_prompt() {
  local user_prompt="$1"
  local response action_id encoded command_text command_status
  response="$(command "$HARNESH_BIN" __agent-prompt "$user_prompt")" || return $?
  while [[ -n "$response" ]]; do
    IFS=$'\t' read -r action_id encoded <<<"$response"
    command_text="$(printf '%s' "$encoded" | base64 --decode)" || return $?
    builtin history -s "$command_text"
    printf '%s%s\n' "${PS1@P}" "$command_text"
    command "$HARNESH_BIN" __marker start "$action_id" agent "$command_text" "$PWD"
    builtin eval -- "$command_text"
    command_status=$?
    command "$HARNESH_BIN" __marker end "$action_id" "$command_status" "$PWD"
    response="$(command "$HARNESH_BIN" __agent-result "$action_id")" || return $?
  done
}

function ,() {
	command "$HARNESH_BIN" __marker prompt "$__harnesh_event_id"
	__harnesh_agent_prompt "$*"
}
function command_not_found_handle() {
	command "$HARNESH_BIN" __marker prompt "$__harnesh_event_id"
	printf '%s\0' "$*" > "$HARNESH_PENDING_PROMPT_FILE"
	return 0
}
PROMPT_COMMAND=__harnesh_postexec
trap '__harnesh_preexec "$BASH_COMMAND"' DEBUG
`
	if _, err := file.WriteString(script); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	return path, nil
}

func writeFishInitFile(_ string) (string, error) {
	file, err := os.CreateTemp("", "harnesh-fish-*")
	if err != nil {
		return "", err
	}
	path := file.Name()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return "", err
	}
	script := `
set -g __harnesh_event_id
set -g __harnesh_event_origin
set -g __harnesh_next_action_id
set -g __harnesh_next_action_command

if functions -q fish_prompt
    functions --copy fish_prompt __harnesh_original_fish_prompt
end

function fish_prompt
    if functions -q __harnesh_original_fish_prompt
        __harnesh_original_fish_prompt
    else
        printf '> '
    end
    if test -n "$__harnesh_next_action_id"
        commandline --replace "$__harnesh_next_action_command"
        commandline --function execute
    end
end

function __harnesh_preexec --on-event fish_preexec
    if test -n "$__harnesh_next_action_id"; and test "$argv[1]" = "$__harnesh_next_action_command"
        set -g __harnesh_event_id $__harnesh_next_action_id
        set -g __harnesh_event_origin agent
        set -g __harnesh_next_action_id
        set -g __harnesh_next_action_command
    else
        set -g __harnesh_event_id "user-$fish_pid-"(random)"-"(random)
        set -g __harnesh_event_origin user
    end
    command $HARNESH_BIN __marker start $__harnesh_event_id $__harnesh_event_origin "$argv[1]" "$PWD"
end

function __harnesh_postexec --on-event fish_postexec
    set -l command_status $status
    set -l completed_id $__harnesh_event_id
    set -l completed_origin $__harnesh_event_origin
    if test -n "$__harnesh_event_id"
        command $HARNESH_BIN __marker end $__harnesh_event_id $command_status "$PWD"
        set -g __harnesh_event_id
        set -g __harnesh_event_origin
    end
    if test "$completed_origin" = agent
        __harnesh_receive_response (command $HARNESH_BIN __agent-result $completed_id | string collect)
    else if test -s "$HARNESH_PENDING_PROMPT_FILE"
        read -z pending_prompt < "$HARNESH_PENDING_PROMPT_FILE"
        printf '' > "$HARNESH_PENDING_PROMPT_FILE"
        if test -n "$pending_prompt"
            __harnesh_receive_response (command $HARNESH_BIN __agent-prompt "$pending_prompt" | string collect)
        end
    end
    return $command_status
end

function __harnesh_receive_response
    set -l response "$argv[1]"
    while test -n "$response"
        set -l parts (string split \t -- "$response")
        set -g __harnesh_next_action_id $parts[1]
        set -g __harnesh_next_action_command (printf '%s' $parts[2] | base64 --decode | string collect)
        return $status
    end
end

function ,
    set -l user_prompt (string join ' ' -- $argv)
    command $HARNESH_BIN __marker prompt $__harnesh_event_id
    __harnesh_receive_response (command $HARNESH_BIN __agent-prompt "$user_prompt" | string collect)
end

function __harnesh_command_not_found --on-event fish_command_not_found
    set -l pending_prompt (string join ' ' -- $argv)
    command $HARNESH_BIN __marker prompt $__harnesh_event_id
    printf '%s\0' "$pending_prompt" > "$HARNESH_PENDING_PROMPT_FILE"
end
`
	if _, err := file.WriteString(script); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	return path, nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func fishQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "\\'") + "'"
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
	return func() { _ = unix.IoctlSetTermios(fd, unix.TCSETS, &original) }, nil
}
