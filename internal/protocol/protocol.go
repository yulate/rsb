// Package protocol defines the wire format for rsb.
//
// P1 architecture: there are two protocol hops, both using the SAME framing.
//
//	agent/CLI  ──(unix socket)──  rsb daemon  ──(ssh)──  rsb-agent (remote)
//
// Both hops are length-delimited JSON. The daemon is a multiplexer: it keeps
// one SSH connection per host open and forwards frames in both directions,
// tagging each with the request id so concurrent requests don't interleave
// ambiguously.
//
// Framing: length-delimited messages.
//
//	+-------------------+--------------------+
//	| 4 bytes (BE u32)  |   payload bytes    |
//	| message length N  |   JSON, N bytes    |
//	+-------------------+--------------------+
//
// JSON payloads may freely contain newlines, quotes, backslashes, etc. — the
// framing is length-delimited, so payload content never affects framing.
//
// The single most important property of this protocol: a command to execute
// is represented as a JSON array of strings (argv), NOT as a shell string.
// The agent runs it with execve(2), so no shell ever parses it. There is
// therefore no quoting/escaping layer to get wrong. See Request.Argv.
package protocol

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// MaxPayload protects against a corrupted length prefix causing a huge
// allocation. 64 MiB is far beyond anything a single command's metadata
// should need; stdout/stderr content is streamed in chunks well under this.
const MaxPayload = 64 << 20

// FrameKind tags a message so the reader can decode it into the right struct.
type FrameKind string

const (
	KindRequest  FrameKind = "request"  // client -> agent: start an exec
	KindStdin    FrameKind = "stdin"    // client -> agent: stream bytes to a running process's stdin
	KindEndStdin FrameKind = "end_stdin" // client -> agent: signal EOF on a process's stdin
	KindCancel   FrameKind = "cancel"   // client -> agent: kill a running process
	KindOutput   FrameKind = "output"   // agent -> client: a chunk of process output
	KindResult   FrameKind = "result"   // agent -> client: process exited
	KindError    FrameKind = "error"    // agent -> client: request could not run
	KindHello    FrameKind = "hello"    // agent -> client: handshake (version, caps)
)

// Frame is the envelope on the wire. Exactly one of Request/Output/Result/
// Error/Hello is populated, selected by Kind.
type Frame struct {
	Kind FrameKind       `json:"kind"`
	Body json.RawMessage `json:"body"`
}

// Request starts an exec. Argv is the canonical representation: never a shell
// string, always an array passed straight to execve.
//
// Stdin for the process is NOT carried inline here (P1 change). Instead the
// client sends zero or more Stdin frames after the Request, then one
// EndStdin frame to signal EOF. This lets the client stream large inputs and
// feed interactive processes. If the client sends no Stdin frames at all and
// no EndStdin, the agent treats stdin as /dev/null (typical for one-shot exec).
type Request struct {
	ID        string            `json:"id"`
	Type      string            `json:"type"`             // currently only "exec"
	Argv      []string          `json:"argv"`             // execve argv, no shell
	Cwd       string            `json:"cwd,omitempty"`    // working dir; "" = session's cwd
	Env       map[string]string `json:"env,omitempty"`    // extra/override env; no expansion
	TimeoutMs int               `json:"timeout_ms,omitempty"`
	Container string            `json:"container,omitempty"` // P2: nsenter/docker
	Session   string            `json:"session,omitempty"`  // P1: session whose cwd/env to inherit

	// StdinClosed indicates the client promises no Stdin frames will follow
	// and the process should get EOF on stdin immediately. Equivalent to the
	// client sending EndStdin right after Request. Convenience for one-shot
	// commands; set true to avoid a round-trip.
	StdinClosed bool `json:"stdin_closed,omitempty"`
}

// Stdin carries a chunk of bytes destined for a running process's stdin.
// The agent routes it to the process identified by ID. Streamed, may repeat.
type Stdin struct {
	ID   string `json:"id"`
	Data []byte `json:"data"`
}

// EndStdin signals that no more Stdin frames will arrive for ID; the agent
// closes the process's stdin (sends EOF). Sent exactly once per request that
// uses stdin, after the last Stdin frame. (If Request.StdinClosed was true,
// the client need not send this separately.)
type EndStdin struct {
	ID string `json:"id"`
}

// Cancel asks the agent to terminate the process for ID. The agent sends
// SIGTERM (then SIGKILL after a grace period) and emits a Result with the
// resulting signal/exit code.
type Cancel struct {
	ID string `json:"id"`
}

// Output is one chunk of process stdout/stderr. Multiple may arrive per
// request; they are ordered and tagged by Stream.
type Output struct {
	ID     string `json:"id"`
	Stream string `json:"stream"` // "stdout" | "stderr"
	Data   []byte `json:"data"`
}

// Result is the terminal outcome of a request.
type Result struct {
	ID         string `json:"id"`
	ExitCode   int    `json:"exit_code"` // -1 if killed/didn't start
	Signal     string `json:"signal,omitempty"`
	DurationMs int64  `json:"duration_ms"`
}

// Error indicates the request could not be executed at all (bad argv, cwd
// missing, binary not found). Distinct from a non-zero exit code, which is a
// normal Result.
type Error struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// Hello is the first frame the agent sends after startup.
type Hello struct {
	Version int      `json:"version"`
	Caps    []string `json:"caps"` // e.g. ["exec","sessions"]; P0: ["exec"]
}

// ProtocolVersion is bumped on breaking wire-format changes.
const ProtocolVersion = 2

// ---- frame I/O ----

// WriteFrame writes one framed message to w. payload must already be the JSON
// body; this wraps it in the Frame envelope and length prefix.
func WriteFrame(w io.Writer, kind FrameKind, body any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal body: %w", err)
	}
	f := Frame{Kind: kind, Body: raw}
	frameJSON, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("marshal frame: %w", err)
	}
	if len(frameJSON) > MaxPayload {
		return fmt.Errorf("frame too large: %d", len(frameJSON))
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(frameJSON)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	if _, err := w.Write(frameJSON); err != nil {
		return err
	}
	return nil
}

// ReadFrame reads one framed message from r and returns the decoded envelope.
// Returns io.EOF on clean end of stream.
func ReadFrame(r io.Reader) (*Frame, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err // io.EOF is fine for callers
	}
	n := binary.BigEndian.Uint32(header[:])
	if n == 0 || int(n) > MaxPayload {
		return nil, fmt.Errorf("invalid frame length %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	var f Frame
	if err := json.Unmarshal(buf, &f); err != nil {
		return nil, fmt.Errorf("unmarshal frame: %w", err)
	}
	return &f, nil
}
