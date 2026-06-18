// Package daemon implements the local rsb daemon: a long-lived process that
// keeps one SSH connection per remote host open and multiplexes client
// requests over it. This is the P1 "one server, many sessions" model,
// mirroring Warp's SSH extension architecture.
//
// Topology:
//
//	client (rsb exec)  ──unix socket──►  daemon  ──ssh──►  rsb-agent (per host)
//	                      (frame router)            (one conn per host, reused)
//
// The daemon is a pure multiplexer: it forwards frames unchanged in both
// directions, routing by request id. It does not interpret Request bodies
// (that's the agent's job). Its value is connection reuse + cwd/session
// persistence across what the client sees as independent invocations.
package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"rsb/internal/paths"
	"rsb/internal/protocol"
)

const localHost = "local"

// Daemon holds the listener and the per-host connection pool.
type Daemon struct {
	mu       sync.Mutex
	conns    map[string]*hostConn // host -> persistent connection to that host's agent
	ln       net.Listener
	clients  sync.WaitGroup // tracks active serveClient goroutines for graceful shutdown
}

// New creates a daemon listening on the unix socket. It removes any stale
// socket file first.
func New() (*Daemon, error) {
	if err := paths.EnsureRuntimeDir(); err != nil {
		return nil, fmt.Errorf("mkdir runtime: %w", err)
	}
	sock := paths.SocketPath()
	// Remove a stale socket from a crashed previous daemon.
	os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", sock, err)
	}
	return &Daemon{conns: make(map[string]*hostConn), ln: ln}, nil
}

// Serve accepts client connections until the listener closes.
func (d *Daemon) Serve() {
	log.Printf("rsb daemon listening on %s", paths.SocketPath())
	for {
		conn, err := d.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Printf("accept: %v", err)
			continue
		}
		d.clients.Add(1)
		go func() {
			defer d.clients.Done()
			d.serveClient(conn)
		}()
	}
}

// Close stops accepting new connections, waits for in-flight client sessions
// to finish (bounded by a timeout so a stuck client can't hang shutdown),
// then drops all host connections. This is the graceful-shutdown path: a
// plain SIGKILL would drop in-flight transfers mid-stream.
func (d *Daemon) Close() error {
	d.ln.Close() // stop accepting; Serve's Accept loop exits

	// Wait for active client sessions with a deadline. A client stuck on a
	// dead host connection (no inactivity timeout of its own) would otherwise
	// block shutdown forever. 10s is generous for clean in-flight ops.
	done := make(chan struct{})
	go func() {
		d.clients.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		log.Printf("daemon: shutdown timeout, forcing close with %d client(s) still active", countActive(&d.clients))
	}

	d.mu.Lock()
	for _, hc := range d.conns {
		hc.close()
	}
	d.conns = make(map[string]*hostConn)
	d.mu.Unlock()
	return nil
}

// countActive is a helper for the shutdown-timeout log; WaitGroup doesn't
// expose its counter directly, so we approximate (0 if Done channel closed).
func countActive(wg *sync.WaitGroup) int {
	// We can't read the counter; return a sentinel. The log is informational.
	return -1
}

// ---- client handling ----

// serveClient services one client connection. A client session starts with
// the client sending a "hello"-like control frame telling us which host it
// wants (AttachHost), then streams Request/Stdin/EndStdin/Cancel frames,
// receiving Output/Result/Error frames back. We route each frame to the
// right host connection by... well, the host is fixed for the whole client
// session, so all its frames go to one hostConn.
func (d *Daemon) serveClient(c net.Conn) {
	defer c.Close()
	hc, err := d.handshake(c)
	if err != nil {
		// Surface the failure to the client as an Error frame so it gets a
		// real reason (SSH auth denied, host unreachable, etc.) instead of a
		// bare EOF. Falls back to just closing if we can't even write the frame.
		reason := err.Error()
		log.Printf("client handshake: %v", reason)
		body, _ := json.Marshal(protocol.Error{Reason: reason})
		writeRawFrame(c, protocol.KindError, body)
		return
	}
	// After handshake, the client <-> hostConn bridge runs to EOF in both
	// directions.
	bridge(c, hc)
}

// AttachHost is the control frame a client sends first to bind its connection
// to a host. After this, all of the client's frames are forwarded to that
// host's agent connection.
type AttachHost struct {
	Host string `json:"host"` // "local" or any ssh host string
}

// handshake reads the AttachHost frame and returns (or creates) the hostConn
// for that host. It also writes back the agent's Hello so the client can
// version-check.
func (d *Daemon) handshake(c net.Conn) (*hostConn, error) {
	f, err := protocol.ReadFrame(c)
	if err != nil {
		return nil, err
	}
	if f.Kind != "attach" {
		return nil, fmt.Errorf("expected attach frame, got %q", f.Kind)
	}
	var att AttachHost
	if err := jsonUnmarshal(f.Body, &att); err != nil {
		return nil, err
	}
	if att.Host == "" {
		return nil, errors.New("attach: empty host")
	}
	hc, err := d.hostConn(att.Host)
	if err != nil {
		return nil, err
	}
	// Forward the agent's Hello to this client.
	hc.mu.Lock()
	helloJSON := hc.helloJSON
	hc.mu.Unlock()
	if len(helloJSON) > 0 {
		// Rewrite the hello frame to the client directly.
		writeRawFrame(c, "hello", helloJSON)
	}
	return hc, nil
}

// ---- host connection pool ----

// hostConn wraps one persistent SSH (or local) connection to an rsb-agent.
// Multiple clients share it; frames are multiplexed by request id.
type hostConn struct {
	host      string
	cmd       *exec.Cmd       // the ssh/local process
	stdin     io.WriteCloser  // write frames to the agent
	stdout    io.Reader       // read frames from the agent
	closeFn   func() error
	helloJSON jsonRaw // cached Hello body to replay to new clients

	mu        sync.Mutex
	routes    map[string]chan *protocol.Frame // id -> per-request reply channel
	clientIn  chan *protocol.Frame            // frames from clients, to send to agent
	closed    bool
}

func (d *Daemon) hostConn(host string) (*hostConn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if hc, ok := d.conns[host]; ok && !hc.isClosed() {
		return hc, nil
	}
	hc, err := openHostConn(host)
	if err != nil {
		return nil, err
	}
	d.conns[host] = hc
	return hc, nil
}

// openHostConn starts the rsb-agent process for the given host (ssh for
// remote, direct spawn for "local") and begins its read pump.
func openHostConn(host string) (*hostConn, error) {
	var cmd *exec.Cmd
	if host == localHost {
		bin := findLocalAgent()
		if bin == "" {
			return nil, errors.New("local rsb-agent not found; build it: go build -o bin/rsb-agent ./cmd/rsb-agent")
		}
		cmd = exec.Command(bin)
	} else {
		cmd = exec.Command("ssh", host, paths.RemoteAgentPath)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// Capture stderr into a buffer during handshake so that if the agent never
	// says Hello (e.g. SSH auth failed, agent binary missing), we can surface
	// the real reason instead of a bare "EOF". After handshake we switch to a
	// line-logging writer for ongoing diagnostics.
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start agent: %w", err)
	}

	hc := &hostConn{
		host:     host,
		cmd:      cmd,
		stdin:    stdin,
		stdout:   stdout,
		closeFn: func() error {
			stdin.Close()
			return cmd.Wait()
		},
		routes:   make(map[string]chan *protocol.Frame),
		clientIn: make(chan *protocol.Frame, 64),
	}

	// Read the agent's Hello before serving traffic; cache it for new clients.
	// On failure, wrap the stderr capture so the client gets the SSH-level
	// reason (auth denied, connection refused, binary not found) rather than
	// a cryptic EOF.
	if err := hc.readHello(); err != nil {
		hc.close()
		return nil, handshakeError(host, err, stderrBuf.String())
	}
	// Hello succeeded: hand stderr off to ongoing line logging.
	cmd.Stderr = logWriter{prefix: "agent[" + host + "]: "}
	// Two pumps: client-frames -> agent stdin, and agent stdout -> routed replies.
	go hc.pumpToAgent()
	go hc.pumpFromAgent()
	return hc, nil
}

// handshakeError turns a readHello failure into a user-actionable error. SSH
// auth failures, refused connections, and missing-agent binaries each get a
// specific, fix-oriented message (pain point #3) instead of "EOF".
func handshakeError(host string, err error, stderr string) error {
	low := strings.ToLower(stderr)
	switch {
	case strings.Contains(low, "permission denied") || strings.Contains(low, "publickey") || strings.Contains(low, "password"):
		return fmt.Errorf("SSH authentication failed for %s\n"+
			"rsb daemon runs ssh non-interactively and cannot prompt for a password.\n"+
			"fix: configure an SSH key (ssh-copy-id %s) or use ssh-agent\n"+
			"detail: %s",
			host, host, strings.TrimSpace(stderr))
	case strings.Contains(low, "connection refused"):
		return fmt.Errorf("connection refused to %s (SSH not running or wrong port?)\ndetail: %s",
			host, strings.TrimSpace(stderr))
	case strings.Contains(low, "could not resolve") || strings.Contains(low, "no such host"):
		return fmt.Errorf("cannot resolve host %s\ndetail: %s", host, strings.TrimSpace(stderr))
	case strings.TrimSpace(stderr) != "":
		return fmt.Errorf("agent handshake failed for %s\n%s", host, strings.TrimSpace(stderr))
	default:
		return fmt.Errorf("agent handshake failed for %s: %w", host, err)
	}
}

func (h *hostConn) readHello() error {
	f, err := protocol.ReadFrame(h.stdout)
	if err != nil {
		return err
	}
	if f.Kind != protocol.KindHello {
		return fmt.Errorf("expected hello, got %q", f.Kind)
	}
	h.mu.Lock()
	h.helloJSON = jsonRaw(f.Body)
	h.mu.Unlock()
	return nil
}

// pumpToAgent drains clientIn and writes each frame to the agent's stdin.
func (h *hostConn) pumpToAgent() {
	for f := range h.clientIn {
		if err := protocol.WriteFrame(h.stdin, f.Kind, f.Body); err != nil {
			if !isClosedErr(err) {
				log.Printf("agent[%s] write: %v", h.host, err)
			}
			return
		}
	}
}

// pumpFromAgent reads frames from the agent's stdout and routes each to the
// client waiting on that request id (via routes[id]). Output frames may also
// carry the id; the client's reply channel is drained until it sees Result
// or Error.
func (h *hostConn) pumpFromAgent() {
	for {
		f, err := protocol.ReadFrame(h.stdout)
		if err != nil {
			if !isClosedErr(err) && !errors.Is(err, io.EOF) {
				log.Printf("agent[%s] read: %v", h.host, err)
			}
			// Tell any in-flight requests their connection is gone, then mark
			// this hostConn closed so the pool rebuilds it on next use. WITHOUT
			// this, a dead connection stays in the pool and every subsequent
			// client hangs forever waiting for a reply that never comes.
			h.failAll("agent connection closed")
			h.close()
			log.Printf("agent[%s] connection closed; will reconnect on next request", h.host)
			return
		}
		// Route by request id for reply kinds.
		id := frameID(f)
		if id == "" {
			continue
		}
		h.mu.Lock()
		ch, ok := h.routes[id]
		// On terminal frames, remove the route after delivery.
		if ok && (f.Kind == protocol.KindResult || f.Kind == protocol.KindError) {
			delete(h.routes, id)
		}
		h.mu.Unlock()
		if ok {
			ch <- f
		}
	}
}

// failAll is called when the agent connection dies: every pending request
// gets an Error so clients don't hang.
func (h *hostConn) failAll(reason string) {
	h.mu.Lock()
	routes := h.routes
	h.routes = make(map[string]chan *protocol.Frame)
	h.mu.Unlock()
	errBody := mustMarshal(protocol.Error{Reason: reason})
	for id, ch := range routes {
		f := &protocol.Frame{Kind: protocol.KindError, Body: errBody}
		_ = id
		ch <- f
	}
}

// send enqueues a client frame for the agent. For Request/Stdin/EndStdin/Cancel
// the frame already carries its id, which the agent uses to route replies.
// The recover guards against the race where close() closes clientIn between
// the isClosed() check and the send (which would panic on a closed channel).
func (h *hostConn) send(f *protocol.Frame) (ok bool) {
	defer func() { recover() }()
	if h.isClosed() {
		return false
	}
	select {
	case h.clientIn <- f:
		return true
	default:
		return false
	}
}

// register reserves a reply channel for id. Returns it; caller drains it
// until Result/Error.
func (h *hostConn) register(id string) chan *protocol.Frame {
	ch := make(chan *protocol.Frame, 16)
	h.mu.Lock()
	h.routes[id] = ch
	h.mu.Unlock()
	return ch
}

func (h *hostConn) unregister(id string) {
	h.mu.Lock()
	delete(h.routes, id)
	h.mu.Unlock()
}

func (h *hostConn) isClosed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

func (h *hostConn) close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	h.mu.Unlock()
	close(h.clientIn)
	if h.closeFn != nil {
		h.closeFn()
	}
}
