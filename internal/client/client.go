// Package client connects to the local rsb daemon, sends an exec request,
// and streams the response to the caller's stdout/stderr. It also lazily
// starts the daemon if the socket isn't serving.
package client

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"rsb/internal/paths"
	"rsb/internal/protocol"
)

// ExecResult is what an exec call returns to the caller.
type ExecResult struct {
	ExitCode int
}

// Exec sends one request to host and streams its output. stdin, if non-nil,
// is streamed to the process via Stdin frames. stdout/stderr receive the
// process's streams. host may be "local" or any ssh host string.
//
// If the daemon isn't running, it's started first. This keeps the agent
// experience transparent: the first `rsb exec` pays the daemon-startup cost,
// subsequent ones reuse it.
func Exec(host string, req *protocol.Request, stdin io.Reader, stdout, stderr io.Writer) (ExecResult, error) {
	conn, err := dialOrStart()
	if err != nil {
		return ExecResult{}, err
	}
	defer conn.Close()

	if err := attach(conn, host); err != nil {
		return ExecResult{}, err
	}
	// The first frame from the daemon is either the agent's Hello (success) or
	// an Error frame if the host connection couldn't be established (SSH auth
	// denied, host unreachable, agent missing). Handle the error case up front
	// so the user sees the real reason, not a later "no result" confusion.
	if err := consumeHandshake(conn, stderr); err != nil {
		return ExecResult{ExitCode: 1}, err
	}

	// Send the request. If stdin is nil or empty, declare it closed upfront
	// so the agent gives the process immediate EOF (no EndStdin round-trip).
	useStdin := stdin != nil
	req.StdinClosed = !useStdin
	if err := protocol.WriteFrame(conn, protocol.KindRequest, req); err != nil {
		return ExecResult{}, fmt.Errorf("send request: %w", err)
	}

	// Pump local stdin -> agent as Stdin frames, then send EndStdin.
	if useStdin {
		done := make(chan struct{})
		go func() {
			pumpStdin(conn, req.ID, stdin)
			protocol.WriteFrame(conn, protocol.KindEndStdin, protocol.EndStdin{ID: req.ID})
			close(done)
		}()
		defer func() { <-done }()
	}

	// Read Output/Result/Error frames, copying to stdout/stderr.
	exitCode := -1
	for {
		f, err := protocol.ReadFrame(conn)
		if err != nil {
			if err == io.EOF {
				return ExecResult{ExitCode: 1}, errors.New("daemon closed connection without result")
			}
			return ExecResult{}, fmt.Errorf("read response: %w", err)
		}
		switch f.Kind {
		case protocol.KindOutput:
			var o protocol.Output
			if err := unmarshal(f.Body, &o); err != nil {
				continue
			}
			if o.Stream == "stdout" {
				stdout.Write(o.Data)
			} else {
				stderr.Write(o.Data)
			}
		case protocol.KindResult:
			var r protocol.Result
			if err := unmarshal(f.Body, &r); err == nil {
				exitCode = r.ExitCode
			}
			return ExecResult{ExitCode: exitCode}, nil
		case protocol.KindError:
			var e protocol.Error
			if err := unmarshal(f.Body, &e); err == nil {
				fmt.Fprintf(stderr, "rsb: agent error: %s\n", e.Reason)
				// P11: a "nsenter ... Permission denied" on a --container request
				// almost always means the remote agent is an OLD version that
				// defaults to nsenter (pre-0.5.0). New agents default to docker
				// exec. Hint the user to upgrade the agent.
				if isStaleAgentContainerError(e.Reason) {
					fmt.Fprint(stderr,
						"rsb: this looks like a STALE remote agent (old versions default to nsenter).\n"+
							"      fix: rsb ensure <host> --force   # upgrades the agent to docker-exec-default\n"+
							"      workaround: rsb exec <host> --argv '[\"docker\",\"exec\",\"<container>\",\"cmd\"]'\n")
				}
			}
			return ExecResult{ExitCode: 1}, nil
		}
	}
}

// isStaleAgentContainerError detects the signature of an old (pre-0.5.0)
// remote agent failing nsenter on a --container request. Those agents default
// to nsenter, which fails for unprivileged users. We can't fix the remote
// agent from here, but we can tell the user what's actually wrong.
func isStaleAgentContainerError(reason string) bool {
	low := strings.ToLower(reason)
	return strings.Contains(low, "nsenter") &&
		(strings.Contains(low, "permission denied") || strings.Contains(low, "operation not permitted"))
}

// dialOrStart connects to the daemon socket, starting the daemon first if the
// socket file doesn't exist. Retries briefly to let the daemon come up.
func dialOrStart() (net.Conn, error) {
	sock := paths.SocketPath()
	conn, err := net.Dial("unix", sock)
	if err == nil {
		return conn, nil
	}
	if !errors.Is(err, os.ErrNotExist) && !isNoSuchFile(err) {
		// Socket exists but won't connect: stale socket from a dead daemon.
		// Remove it and fall through to start a fresh daemon.
		os.Remove(sock)
	}
	if err := startDaemon(); err != nil {
		return nil, fmt.Errorf("start daemon: %w", err)
	}
	// Wait for the daemon to start listening.
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.Dial("unix", sock)
		if err == nil {
			return conn, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("daemon didn't come up: %w", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// startDaemon spawns rsb-daemon detached from this process so it outlives the
// current command (the whole point of connection reuse).
func startDaemon() error {
	if err := paths.EnsureRuntimeDir(); err != nil {
		return fmt.Errorf("mkdir runtime: %w", err)
	}
	bin := findDaemonBinary()
	if bin == "" {
		return errors.New("rsb-daemon binary not found; build it: go build -o bin/rsb-daemon ./cmd/rsb-daemon")
	}
	// Detach: own process group, no inherited stdio. Logs go to a file.
	logFile, err := os.OpenFile(paths.RuntimeDir()+"/daemon.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	cmd := exec.Command(bin)
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = detachAttr()
	return cmd.Start()
}

func attach(c net.Conn, host string) error {
	return protocol.WriteFrame(c, "attach", struct {
		Host string `json:"host"`
	}{Host: host})
}

// consumeHandshake reads the first frame after attach: it should be the
// agent's Hello. If the daemon instead sent an Error frame (host connection
// failed), we extract the reason — which carries the SSH-level detail thanks
// to handshakeError() on the daemon side — and return it for the user.
func consumeHandshake(c net.Conn, stderr io.Writer) error {
	f, err := protocol.ReadFrame(c)
	if err != nil {
		return fmt.Errorf("connect to daemon: %w", err)
	}
	if f.Kind == protocol.KindError {
		var e protocol.Error
		if err := jsonUnmarshal(f.Body, &e); err == nil && e.Reason != "" {
			fmt.Fprintf(stderr, "rsb: %s\n", e.Reason)
			return fmt.Errorf("%s", e.Reason)
		}
		return errors.New("daemon rejected connection")
	}
	// Hello (or anything else): drain and proceed.
	return nil
}

func pumpStdin(c net.Conn, id string, r io.Reader) {
	buf := make([]byte, 8*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			protocol.WriteFrame(c, protocol.KindStdin, protocol.Stdin{ID: id, Data: chunk})
		}
		if err != nil {
			return
		}
	}
}

func unmarshal(b []byte, v any) error { return jsonUnmarshal(b, v) }
