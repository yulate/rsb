// Command rsb-agent is the remote-side daemon for rsb.
//
// P1 architecture: rsb-agent is a long-lived process spawned over SSH by the
// local rsb daemon. It multiplexes many concurrent requests over its single
// stdin/stdout channel, keyed by request id. It also maintains a session
// table so commands in the same session inherit cwd/env from each other.
//
// Lifecycle of a request:
//  1. client sends Request{id, argv, session, cwd, env, stdin_closed}
//  2. agent resolves cwd (session's or request's), builds env, execve
//  3. agent streams Output{id, stdout|stderr} frames as data arrives
//  4. if the process reads stdin, client streams Stdin{id} frames then
//     EndStdin{id}; the agent pipes them in
//  5. client may send Cancel{id} to kill it early
//  6. agent sends Result{id, exit_code} when the process exits
//
// As in P0: argv is executed via execve and NEVER passed to a shell, so
// quotes/escapes/$-expansion cannot be wrong — there is no parser to be
// wrong about.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"rsb/internal/docker"
	"rsb/internal/protocol"
)

// version is injected at build time via ldflags. The agent reports it on
// `rsb-agent --version` so clients can verify the remote agent matches.
var agentVersion = "0.0.0-dev"

// buildTime is injected at build time; helps distinguish same-version rebuilds.
var buildTime = "unknown"

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetPrefix("rsb-agent: ")

	// Handle --version before entering the protocol loop. This lets `rsb
	// agent-version <host>` run `ssh host rsb-agent --version` and get a clean
	// single-line answer on stdout instead of protocol noise.
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Printf("rsb-agent %s (protocol v%d) built %s\n",
			agentVersion, protocol.ProtocolVersion, buildTime)
		return
	}

	a := &agent{
		sessions: make(map[string]*session),
		running:  make(map[string]*runningReq),
	}
	a.run(os.Stdin, os.Stdout)
}

// session holds persistent cwd/env across commands that share a session id.
// Its mutex serializes commands in the same session: a `cd /app` followed by
// `ls` must observe the cd's effect, so same-session requests run one at a
// time. Different sessions run concurrently with each other.
type session struct {
	mu  sync.Mutex
	cwd string
	env map[string]string
}

// runningReq tracks a process in flight so Stdin/Cancel frames can reach it.
// The stdin channel decouples frame arrival from process startup: a Stdin
// frame may arrive before cmd.StdinPipe is wired up, so we buffer frames in
// the channel and a pump goroutine drains them into the pipe once it exists.
type runningReq struct {
	stdinCh chan []byte // stdin chunks; closed-signaled via eofCh
	eofCh   chan struct{}
	eofOnce sync.Once // guards eofCh close (EndStdin + finish race)
	cancel  context.CancelFunc
}

// newRunningReq creates a runningReq registered in the agent's running map
// BEFORE the handleExec goroutine starts. This closes the race where a Stdin
// frame arrives in the main loop before the goroutine has registered itself.
func (a *agent) newRunningReq(id string, cancel context.CancelFunc) *runningReq {
	rr := &runningReq{
		stdinCh: make(chan []byte, 16),
		eofCh:   make(chan struct{}),
		cancel:  cancel,
	}
	a.mu.Lock()
	a.running[id] = rr
	a.mu.Unlock()
	return rr
}

// agent is the server state.
type agent struct {
	mu       sync.Mutex          // guards sessions and running
	sessions map[string]*session
	running  map[string]*runningReq

	pending sync.WaitGroup // tracks in-flight handleExec goroutines for clean shutdown

	outMu sync.Mutex // serializes all frame writes to stdout
	out   io.Writer
}

func (a *agent) run(stdin io.Reader, stdout io.Writer) {
	a.out = stdout

	// Hello first so the daemon can version-check before sending requests.
	a.writeFrame(protocol.KindHello, protocol.Hello{
		Version: protocol.ProtocolVersion,
		Caps:    []string{"exec", "sessions", "stream_stdin", "cancel", "files"},
	})

	// Single reader loop. All frames (Request, Stdin, EndStdin, Cancel) arrive
	// here and are dispatched. Output/Result frames are written back from the
	// per-request goroutines, serialized through outMu.
	for {
		f, err := protocol.ReadFrame(stdin)
		if err != nil {
			if errors.Is(err, io.EOF) {
				// Don't exit immediately: in-flight requests still need to
				// finish and emit their Result frames. Wait for every
				// handleExec goroutine to complete, then go.
				a.pending.Wait()
				log.Printf("stdin closed, exiting")
				return
			}
			log.Fatalf("read frame: %v", err)
		}
		switch f.Kind {
		case protocol.KindRequest:
			var req protocol.Request
			if err := json.Unmarshal(f.Body, &req); err != nil {
				log.Printf("bad request body: %v", err)
				continue
			}
			switch req.Type {
			case "exec", "":
				a.dispatchExec(req)
			case "file_stat":
				a.handleFileStat(req)
			case "file_get":
				go a.handleFileGet(req)
			case "file_put":
				// Run SYNCHRONOUSLY in the read loop, NOT in a goroutine. A
				// file_put streams FileChunk frames that the main loop must
				// read one at a time; if we ran it async with a channel, the
				// channel buffer would fill and deadlock against the read loop.
				// Synchronous = the loop naturally backpressures: read chunk,
				// write to disk, read next. No other request runs during a
				// put, which is fine (cp/sync are exclusive operations).
				a.handleFilePutSync(req, stdin)
			default:
				a.writeFrame(protocol.KindError, protocol.Error{
					ID: req.ID, Reason: "unsupported request type: " + req.Type,
				})
			}
		case protocol.KindStdin:
			var s protocol.Stdin
			if err := json.Unmarshal(f.Body, &s); err == nil {
				a.feedStdin(s.ID, s.Data)
			}
		case protocol.KindEndStdin:
			var e protocol.EndStdin
			if err := json.Unmarshal(f.Body, &e); err == nil {
				a.closeStdin(e.ID)
			}
		case protocol.KindCancel:
			var c protocol.Cancel
			if err := json.Unmarshal(f.Body, &c); err == nil {
				a.cancelReq(c.ID)
			}
		default:
			log.Printf("ignoring unexpected frame kind=%q", f.Kind)
		}
	}
}

// ---- frame output helper ----

func (a *agent) writeFrame(kind protocol.FrameKind, body any) {
	a.outMu.Lock()
	defer a.outMu.Unlock()
	if err := protocol.WriteFrame(a.out, kind, body); err != nil {
		log.Printf("write %s frame: %v", kind, err)
	}
}

// ---- session management ----

// resolveSession returns the session for req, creating it if needed. The
// returned session's cwd/env are used as the base for this command; the
// request's own cwd/env (if set) override for this invocation and, on
// success, persist back into the session so the next command inherits them.
func (a *agent) resolveSession(req protocol.Request) *session {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[req.Session]
	if !ok {
		s = &session{env: map[string]string{}}
		if req.Session != "" {
			a.sessions[req.Session] = s
		}
	}
	return s
}

// persistSession updates the session's cwd/env after a successful exec, so
// the next command in the same session inherits them (e.g. `cd /app` sticks).
// We only persist cwd if the command's cwd was set explicitly (a `cd`-like
// effect via the --cwd flag), and env additions from the request.
func (a *agent) persistSession(req protocol.Request, effectiveCwd string) {
	if req.Session == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[req.Session]
	if !ok {
		return
	}
	if req.Cwd != "" {
		s.cwd = effectiveCwd
	}
	for k, v := range req.Env {
		s.env[k] = v
	}
}

// ---- exec handling ----

// dispatchExec is called from the main read loop. It does the synchronous
// setup that must happen BEFORE the goroutine starts (so Stdin/Cancel frames
// arriving next can find the request), then launches handleExec. Errors that
// are detectable without starting a process are reported inline.
func (a *agent) dispatchExec(req protocol.Request) {
	if req.Type != "exec" && req.Type != "" {
		a.writeFrame(protocol.KindError, protocol.Error{ID: req.ID, Reason: "unsupported type: " + req.Type})
		return
	}
	if len(req.Argv) == 0 {
		a.writeFrame(protocol.KindError, protocol.Error{ID: req.ID, Reason: "argv is empty"})
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	if req.TimeoutMs > 0 {
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutMs)*time.Millisecond)
	}
	// Register the runningReq synchronously so a Stdin frame in the very next
	// loop iteration finds it even before the goroutine has wired the pipe.
	rr := a.newRunningReq(req.ID, cancel)

	a.pending.Add(1)
	go func() {
		defer a.pending.Done()
		defer cancel()
		defer a.unregisterRunning(req.ID)
		a.handleExec(ctx, req, rr)
	}()
}

func (a *agent) handleExec(ctx context.Context, req protocol.Request, rr *runningReq) {
	sess := a.resolveSession(req)

	// Serialize commands within the same session: `cd /app` then `ls` must see
	// the cd's effect. Different sessions (and no-session requests) run free.
	sess.mu.Lock()
	defer sess.mu.Unlock()

	// Effective cwd: request overrides, else session's, else agent's own.
	effectiveCwd := req.Cwd
	if effectiveCwd == "" {
		effectiveCwd = sess.cwd
	}

	// Built-in `cd`: a shell built-in can't be execve'd (there's no `cd`
	// binary), so the agent implements it directly — updating the session's
	// cwd so subsequent commands in the session inherit it. This is what makes
	// `rsb exec host --session s --argv '["cd","/app"]'` followed by
	// `["pwd"]` Just Work, matching how a user expects an interactive shell
	// to behave. Only honored when a session is in use (cd without persistence
	// is meaningless).
	if req.Session != "" && len(req.Argv) > 0 && req.Argv[0] == "cd" {
		target := effectiveCwd
		if len(req.Argv) > 1 {
			target = req.Argv[1]
		}
		abs := resolveCwd(target, effectiveCwd)
		if abs == "" {
			a.writeFrame(protocol.KindError, protocol.Error{ID: req.ID, Reason: "cd: cannot resolve " + target})
			return
		}
		if _, err := os.Stat(abs); err != nil {
			a.writeFrame(protocol.KindError, protocol.Error{ID: req.ID, Reason: "cd: " + err.Error()})
			return
		}
		sess.cwd = abs
		a.writeFrame(protocol.KindResult, protocol.Result{ID: req.ID, ExitCode: 0, DurationMs: 0})
		return
	}

	// Effective env: session's base, then request's overrides.
	envMap := map[string]string{}
	for k, v := range sess.env {
		envMap[k] = v
	}
	for k, v := range req.Env {
		envMap[k] = v
	}

	// Effective argv: if a container is requested, route through the docker
	// adapter which rewrites argv into nsenter/docker-exec form — still an
	// argv array reaching execve, no shell joining. Without a container, the
	// caller's argv runs on the host directly.
	effectiveArgv := req.Argv
	if req.Container != "" {
		rewritten, err := docker.BuildArgv(req.Container, req.Argv)
		if err != nil {
			a.writeFrame(protocol.KindError, protocol.Error{
				ID: req.ID, Reason: "container: " + err.Error(),
			})
			return
		}
		effectiveArgv = rewritten
	}

	bin, err := exec.LookPath(effectiveArgv[0])
	if err != nil {
		a.writeFrame(protocol.KindError, protocol.Error{
			ID: req.ID, Reason: "executable not found in PATH: " + effectiveArgv[0],
		})
		return
	}

	envList := os.Environ()
	for k, v := range envMap {
		envList = append(envList, k+"="+v)
	}

	cmd := exec.CommandContext(ctx, bin, effectiveArgv[1:]...)
	cmd.Args = append([]string{}, effectiveArgv...)
	cmd.Env = envList
	if effectiveCwd != "" && req.Container == "" {
		// cwd inside a container is the container's concern; the host cwd
		// setting would be meaningless. Only apply cwd for host execution.
		cmd.Dir = effectiveCwd
	}

	// Wire stdin. If the client declared stdin_closed, give the process an
	// immediate EOF. Otherwise create a pipe and pump chunks from rr.stdinCh
	// into it; this decouples frame arrival (in the main loop) from process
	// startup (here), so a Stdin frame can't be lost to a race.
	if req.StdinClosed {
		cmd.Stdin = bytes.NewReader(nil)
	} else {
		w, err := cmd.StdinPipe()
		if err != nil {
			a.writeFrame(protocol.KindError, protocol.Error{ID: req.ID, Reason: "stdin pipe: " + err.Error()})
			return
		}
		go pumpStdin(w, rr)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		a.writeFrame(protocol.KindError, protocol.Error{ID: req.ID, Reason: "stdout pipe: " + err.Error()})
		return
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		a.writeFrame(protocol.KindError, protocol.Error{ID: req.ID, Reason: "stderr pipe: " + err.Error()})
		return
	}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		a.writeFrame(protocol.KindError, protocol.Error{ID: req.ID, Reason: "start: " + err.Error()})
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go a.streamPipe(req.ID, "stdout", stdoutPipe, &wg)
	go a.streamPipe(req.ID, "stderr", stderrPipe, &wg)

	waitErr := cmd.Wait()

	// Close stdin pipe if open (process may have exited without reading all).
	// Signal the stdin pump to stop (process is done) and wait for output
	// streamers before computing the result, so all Output frames precede
	// the Result frame on the wire.
	signalEOF(rr)
	wg.Wait()

	res := protocol.Result{
		ID:         req.ID,
		ExitCode:   -1,
		DurationMs: time.Since(start).Milliseconds(),
	}
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		res.Signal = "TIMEOUT"
	case waitErr != nil:
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			res.ExitCode = ee.ExitCode()
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				res.Signal = ws.Signal().String()
			}
		} else {
			res.ExitCode = 1
		}
	default:
		res.ExitCode = 0
	}

	a.writeFrame(protocol.KindResult, res)

	// Persist session state only on successful execution (cwd/env sticks).
	if res.ExitCode == 0 {
		a.persistSession(req, effectiveCwd)
	}
}

// ---- stdin / cancel routing ----

// unregisterRunning drops a finished request from the running map. The
// runningReq itself was created and registered synchronously in dispatchExec
// via newRunningReq, so Stdin/Cancel frames always find their target.
func (a *agent) unregisterRunning(id string) {
	a.mu.Lock()
	delete(a.running, id)
	a.mu.Unlock()
}

// feedStdin enqueues a stdin chunk onto the request's channel. Non-blocking:
// if the buffer is full (slow process), we log and drop rather than block the
// main read loop, which would stall every other request.
func (a *agent) feedStdin(id string, data []byte) {
	a.mu.Lock()
	rr, ok := a.running[id]
	a.mu.Unlock()
	if !ok {
		return
	}
	select {
	case rr.stdinCh <- data:
	default:
		log.Printf("WARN: stdin buffer full for %s, dropping %d bytes", id, len(data))
	}
}

// closeStdin signals EOF to the request's stdin pump, which closes the process
// pipe. Idempotent.
func (a *agent) closeStdin(id string) {
	a.mu.Lock()
	rr, ok := a.running[id]
	a.mu.Unlock()
	if !ok {
		return
	}
	signalEOF(rr)
}

// signalEOF closes rr.eofCh once, safely. Called both when the client sends
// EndStdin and when handleExec finishes — whichever first.
func signalEOF(rr *runningReq) {
	rr.eofOnce.Do(func() { close(rr.eofCh) })
}

func (a *agent) cancelReq(id string) {
	a.mu.Lock()
	rr, ok := a.running[id]
	a.mu.Unlock()
	if ok {
		rr.cancel()
		log.Printf("cancelled %s", id)
	}
}

// pumpStdin drains rr.stdinCh into the process's stdin pipe writer, and closes
// the pipe when eofCh fires (client sent EndStdin, or the command finished).
//
// Ordering matters: frames arrive on a single ordered channel, so by the time
// EndStdin reaches eofCh, every preceding Stdin frame has already been fed
// into stdinCh. We must NOT let select drop those buffered chunks — a naive
// `select { case chunk; case eof }` can pick eof and lose pending data, which
// is exactly why `docker exec -i ... < file` produced no output. So once eof
// fires we keep writing until stdinCh is empty, THEN close.
func pumpStdin(w io.WriteCloser, rr *runningReq) {
	defer w.Close()
	eof := false
	for {
		// Once EOF was signaled, drain anything still buffered in stdinCh
		// before closing the pipe. Non-blocking drain each iteration.
		if eof {
			drainStdinCh(w, rr.stdinCh)
			return
		}
		select {
		case chunk, ok := <-rr.stdinCh:
			if !ok {
				return
			}
			if _, err := w.Write(chunk); err != nil && !isClosedPipeErr(err) {
				log.Printf("write process stdin: %v", err)
				return
			}
		case <-rr.eofCh:
			eof = true
		}
	}
}

// drainStdinCh writes every currently-buffered chunk in ch to w, then returns.
// Called after EOF so no pending stdin data is lost when the pipe closes.
func drainStdinCh(w io.Writer, ch chan []byte) {
	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				return
			}
			if _, err := w.Write(chunk); err != nil && !isClosedPipeErr(err) {
				return
			}
		default:
			return
		}
	}
}

// ---- output streaming ----

func (a *agent) streamPipe(id, stream string, r io.Reader, wg *sync.WaitGroup) {
	defer wg.Done()
	buf := make([]byte, 8*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			a.writeFrame(protocol.KindOutput, protocol.Output{
				ID: id, Stream: stream, Data: chunk,
			})
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !isClosedPipeErr(err) {
				log.Printf("read %s: %v", stream, err)
			}
			return
		}
	}
}

// isClosedPipeErr reports whether err is the benign "file already closed" /
// "broken pipe" family from exec.Cmd pipe teardown.
func isClosedPipeErr(err error) bool {
	s := err.Error()
	return strings.Contains(s, "file already closed") ||
		strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "closed")
}

// resolveCwd turns a `cd` target into an absolute path: handles absolute
// paths, paths relative to the current cwd, and ~ (HOME). Returns "" if the
// target can't be resolved (e.g. bad HOME).
func resolveCwd(target, curCwd string) string {
	if target == "" {
		return ""
	}
	// ~ expansion
	if target == "~" || strings.HasPrefix(target, "~/") {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return ""
		}
		if target == "~" {
			return home
		}
		return home + target[1:]
	}
	if strings.HasPrefix(target, "/") {
		return target
	}
	if curCwd == "" {
		var err error
		curCwd, err = os.Getwd()
		if err != nil {
			return ""
		}
	}
	return curCwd + "/" + target
}
