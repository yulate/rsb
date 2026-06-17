package client

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"rsb/internal/protocol"
)

var replCounter uint64

// replID generates a process-unique id for each repl request.
func replID() string {
	n := atomic.AddUint64(&replCounter, 1)
	return fmt.Sprintf("repl-%d-%d", time.Now().UnixNano(), n)
}

// Repl runs an interactive read-eval-print loop against a host over a single
// daemon connection. All commands share one session so cwd/env persist across
// inputs — `cd /app` then `ls` behaves like a real shell. Input formats:
//
//	["argv","as","json"]           full argv array (recommended; zero ambiguity)
//	.argv as json                  leading dot: same, terser
//	:session NAME                  switch/rename the active session
//	:container NAME                set a container for subsequent commands
//	:container                     clear container (run on host)
//	:quit                          exit
//
// Everything else is read verbatim as a single-arg argv (["sh","-c",line]) so
// users can type casual shell-like commands quickly; but for any input with
// quotes/spaces/special chars the explicit JSON form is the safe one.
func Repl(host string, session string, in io.Reader, out, errw io.Writer) int {
	conn, err := dialOrStart()
	if err != nil {
		fmt.Fprintf(errw, "rsb: %v\n", err)
		return 1
	}
	defer conn.Close()
	if err := attach(conn, host); err != nil {
		fmt.Fprintf(errw, "rsb: attach: %v\n", err)
		return 1
	}
	// Drain the daemon-forwarded Hello.
	if _, err := protocol.ReadFrame(conn); err != nil {
		fmt.Fprintf(errw, "rsb: read hello: %v\n", err)
		return 1
	}

	container := ""
	prompt := func() {
		where := host
		if host == "local" {
			where = "local"
		}
		if container != "" {
			where += "/" + container
		}
		if session != "" {
			fmt.Fprintf(out, "[%s:%s] ", where, session)
		} else {
			fmt.Fprintf(out, "[%s] ", where)
		}
	}

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	prompt()
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			prompt()
			continue
		}
		// Meta commands.
		if strings.HasPrefix(line, ":") {
			args := strings.SplitN(line[1:], " ", 2)
			switch args[0] {
			case "quit", "exit":
				return 0
			case "session":
				if len(args) > 1 {
					session = strings.TrimSpace(args[1])
				}
				prompt()
				continue
			case "container":
				if len(args) > 1 {
					container = strings.TrimSpace(args[1])
				} else {
					container = ""
				}
				prompt()
				continue
			default:
				fmt.Fprintf(errw, "unknown command: :%s\n", args[0])
				prompt()
				continue
			}
		}

		argv, err := parseArgv(line)
		if err != nil {
			fmt.Fprintf(errw, "rsb: %v\n", err)
			prompt()
			continue
		}
		req := &protocol.Request{
			ID:          replID(),
			Type:        "exec",
			Argv:        argv,
			Session:     session,
			StdinClosed: true,
			Container:   container,
		}
		if err := protocol.WriteFrame(conn, protocol.KindRequest, req); err != nil {
			fmt.Fprintf(errw, "rsb: send: %v\n", err)
			return 1
		}
		// Stream this command's frames until Result/Error.
		runOneCommand(conn, out, errw)
		prompt()
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintf(errw, "rsb: read input: %v\n", err)
	}
	return 0
}

// parseArgv interprets one input line into an argv array. JSON array form is
// authoritative; otherwise wrap the raw line as sh -c (quick casual mode).
func parseArgv(line string) ([]string, error) {
	trimmed := strings.TrimPrefix(line, ".")
	trimmed = strings.TrimSpace(trimmed)
	if strings.HasPrefix(trimmed, "[") {
		var argv []string
		if err := json.Unmarshal([]byte(trimmed), &argv); err != nil {
			return nil, fmt.Errorf("invalid argv JSON: %w", err)
		}
		if len(argv) == 0 {
			return nil, fmt.Errorf("empty argv")
		}
		return argv, nil
	}
	// Casual mode: run the line through sh -c. Note this reintroduces a shell
	// for the *content* of that one line — but only that line, and only
	// because the user chose the shorthand. The explicit JSON form is the
	// quoting-safe path and is what SKILL.md teaches agents to use.
	return []string{"sh", "-c", line}, nil
}

// runOneCommand reads Output/Result/Error frames for the in-flight request and
// copies output to out/errw. Stops at the terminal frame.
func runOneCommand(conn net.Conn, out, errw io.Writer) {
	for {
		f, err := protocol.ReadFrame(conn)
		if err != nil {
			fmt.Fprintf(errw, "rsb: read: %v\n", err)
			return
		}
		switch f.Kind {
		case protocol.KindOutput:
			var o protocol.Output
			if err := jsonUnmarshal(f.Body, &o); err != nil {
				continue
			}
			if o.Stream == "stdout" {
				out.Write(o.Data)
			} else {
				errw.Write(o.Data)
			}
		case protocol.KindResult:
			var r protocol.Result
			if err := jsonUnmarshal(f.Body, &r); err == nil {
				if r.ExitCode != 0 {
					fmt.Fprintf(errw, "[exit %d]\n", r.ExitCode)
				}
			}
			return
		case protocol.KindError:
			var e protocol.Error
			if err := jsonUnmarshal(f.Body, &e); err == nil {
				fmt.Fprintf(errw, "rsb: agent: %s\n", e.Reason)
			}
			return
		}
	}
}
