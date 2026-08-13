package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func main() {
	if code := run(os.Args[1:]); code != 0 {
		os.Exit(code)
	}
}

func run(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "__marker":
			if err := writeMarker(args[1:], os.Stdout); err != nil {
				fmt.Fprintf(os.Stderr, "harnesh: %v\n", err)
				return 2
			}
			return 0
		case "__agent-prompt":
			return runAgentPromptHelper(args[1:])
		case "__agent-result":
			return runAgentResultHelper(args[1:])
		case "sessions":
			return runSessions(args[1:])
		case "history":
			return runHistory(args[1:])
		case "-h", "--help", "help":
			printUsage(os.Stdout)
			return 0
		}
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "harnesh: %v\n", err)
		return 1
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell, err = exec.LookPath("bash")
		if err != nil {
			fmt.Fprintln(os.Stderr, "harnesh: SHELL is unset and bash is unavailable")
			return 1
		}
	}
	var j *journal
	if len(args) == 0 {
		j, err = newSession(cwd, shell)
	} else if args[0] == "resume" {
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: harnesh resume SESSION_ID|--last")
			return 2
		}
		id := args[1]
		if id == "--last" {
			id, err = lastSessionID()
		}
		if err == nil {
			j, err = openSession(id)
		}
		if err == nil {
			cwd = selectResumeCWD(j.lastCWD(), cwd, os.Stderr)
		}
	} else {
		fmt.Fprintf(os.Stderr, "harnesh: unknown command %q\n", args[0])
		printUsage(os.Stderr)
		return 2
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "harnesh: %v\n", err)
		return 1
	}
	if err := j.migrateLegacyCodexAdapter(); err != nil {
		fmt.Fprintf(os.Stderr, "harnesh: %v\n", err)
		return 1
	}
	adapter := newAgentBridge()
	controller := newAgentController(adapter, j, os.Stdout)
	fmt.Fprintf(os.Stderr, "harnesh: session %s\n", j.sessionID())
	if err := runShell(j, controller, cwd); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "harnesh: %v\n", err)
		return 1
	}
	return 0
}

func selectResumeCWD(recorded, fallback string, warnings *os.File) string {
	if info, err := os.Stat(recorded); err == nil && info.IsDir() {
		return recorded
	}
	fmt.Fprintf(warnings, "harnesh: recorded cwd %s is unavailable; using %s\n", recorded, fallback)
	return fallback
}

func runSessions(args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "usage: harnesh sessions")
		return 2
	}
	sessions, err := listSessions()
	if err != nil {
		fmt.Fprintf(os.Stderr, "harnesh: %v\n", err)
		return 1
	}
	for _, session := range sessions {
		sessionID := session.Adapters["agent"].ThreadID
		if sessionID == "" {
			sessionID = session.Adapters["codex"].ThreadID
			if sessionID != "" {
				sessionID = "codex:" + sessionID
			}
		}
		fmt.Printf("%s\t%s\t%s\t%s\n", session.ID, session.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"), sessionID, session.LastCWD)
	}
	return 0
}

func runHistory(args []string) int {
	sessionID, rest, err := parseSessionOption(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "harnesh: %v\n", err)
		return 2
	}
	if sessionID == "" {
		sessionID = os.Getenv("HARNESH_SESSION_ID")
	}
	if sessionID == "" {
		sessionID, err = lastSessionID()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "harnesh: %v\n", err)
		return 1
	}
	j, err := openSession(sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "harnesh: %v\n", err)
		return 1
	}
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "usage: harnesh history tail [COUNT] [--session ID] | harnesh history output EVENT_ID [--session ID]")
		return 2
	}
	switch rest[0] {
	case "tail":
		count := 20
		if len(rest) == 2 {
			count, err = strconv.Atoi(rest[1])
			if err != nil || count < 1 {
				fmt.Fprintln(os.Stderr, "harnesh: history count must be a positive integer")
				return 2
			}
		} else if len(rest) != 1 {
			fmt.Fprintln(os.Stderr, "usage: harnesh history tail [COUNT] [--session ID]")
			return 2
		}
		events, readErr := j.events()
		if readErr != nil {
			fmt.Fprintf(os.Stderr, "harnesh: %v\n", readErr)
			return 1
		}
		if len(events) > count {
			events = events[len(events)-count:]
		}
		encoder := json.NewEncoder(os.Stdout)
		for _, event := range events {
			if err := encoder.Encode(event); err != nil {
				fmt.Fprintf(os.Stderr, "harnesh: %v\n", err)
				return 1
			}
		}
		return 0
	case "output":
		if len(rest) != 2 || !validID(rest[1]) {
			fmt.Fprintln(os.Stderr, "usage: harnesh history output EVENT_ID [--session ID]")
			return 2
		}
		if err := copyEventOutput(os.Stdout, j, rest[1]); err != nil {
			fmt.Fprintf(os.Stderr, "harnesh: %v\n", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "harnesh: unknown history command %q\n", rest[0])
		return 2
	}
}

func parseSessionOption(args []string) (string, []string, error) {
	var sessionID string
	rest := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		if args[index] == "--session" {
			if sessionID != "" || index+1 >= len(args) {
				return "", nil, errors.New("--session requires one session ID")
			}
			sessionID = args[index+1]
			index++
			continue
		}
		if strings.HasPrefix(args[index], "--session=") {
			if sessionID != "" {
				return "", nil, errors.New("--session was specified more than once")
			}
			sessionID = strings.TrimPrefix(args[index], "--session=")
			continue
		}
		rest = append(rest, args[index])
	}
	if sessionID != "" && !validID(sessionID) {
		return "", nil, fmt.Errorf("invalid session ID %q", sessionID)
	}
	return sessionID, rest, nil
}

func printUsage(output *os.File) {
	name := filepath.Base(os.Args[0])
	fmt.Fprintf(output, `Usage:
  %s
  %s resume SESSION_ID|--last
  %s sessions
  %s history tail [COUNT] [--session SESSION_ID]
  %s history output EVENT_ID [--session SESSION_ID]

Start a persistent shell that dispatches prose prompts through agent.
Use ", PROMPT" to force an agent prompt. Unknown commands retain the existing
command-not-found prompt path.
`, name, name, name, name, name)
}
