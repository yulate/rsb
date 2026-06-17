package daemon

import (
	"encoding/json"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"rsb/internal/paths"
	"rsb/internal/protocol"
)

// jsonRaw is a json.RawMessage alias for caching frame bodies.
type jsonRaw = json.RawMessage

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
func mustMarshal(v any) jsonRaw           { b, _ := json.Marshal(v); return b }

// frameID extracts the request id from any frame body that has one.
func frameID(f *protocol.Frame) string {
	var probe struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(f.Body, &probe)
	return probe.ID
}

// writeRawFrame writes a frame to w with an already-serialized body. Used to
// replay the cached Hello to a new client without re-decoding.
func writeRawFrame(w io.Writer, kind protocol.FrameKind, body jsonRaw) {
	protocol.WriteFrame(w, kind, body)
}

// isClosedErr reports benign pipe/socket-closed errors that occur on shutdown.
func isClosedErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "closed") ||
		strings.Contains(s, "broken pipe")
}

// logWriter turns anything written to it into log lines. Used to surface the
// agent's stderr without polluting the protocol channels.
type logWriter struct{ prefix string }

func (l logWriter) Write(p []byte) (int, error) {
	log.Printf("%s%s", l.prefix, strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// findLocalAgent locates the rsb-agent binary for local mode via the install
// home discovery chain (RSB_HOME > executable path > cwd), then PATH.
// Mirrors how the client finds rsb-daemon — no cwd dependence.
func findLocalAgent() string {
	if dir, err := paths.LocalPlatformDir(); err == nil {
		p := filepath.Join(dir, "rsb-agent")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	if p, err := exec.LookPath("rsb-agent"); err == nil {
		return p
	}
	return ""
}

// bridge connects a client connection to its hostConn for the lifetime of the
// client session. It reads frames from the client and:
//   - Request: registers a reply route, forwards to agent, then spawns a
//     pump that copies the agent's replies (Output*/Result/Error for that id)
//     back to the client. This lets multiple client requests run concurrently.
//   - Stdin/EndStdin/Cancel: forwarded to the agent immediately (the agent
//     already has the runningReq registered for the id).
//
// The function returns when the client disconnects (read EOF).
func bridge(c net.Conn, hc *hostConn) {
	// writer serializes frame writes back to the client socket.
	type writeReq struct {
		frame *protocol.Frame
		err   chan error
	}
	writeCh := make(chan writeReq, 64)
	go func() {
		for w := range writeCh {
			err := protocol.WriteFrame(c, w.frame.Kind, w.frame.Body)
			w.err <- err
		}
	}()
	sendBack := func(f *protocol.Frame) {
		errCh := make(chan error, 1)
		writeCh <- writeReq{frame: f, err: errCh}
		<-errCh
	}

	for {
		f, err := protocol.ReadFrame(c)
		if err != nil {
			if !isClosedErr(err) && err != io.EOF {
				log.Printf("client read: %v", err)
			}
			break
		}
		id := frameID(f)
		switch f.Kind {
		case protocol.KindRequest:
			// Register a reply channel keyed by id, forward the request, and
			// pump replies back until the terminal frame arrives.
			replyCh := hc.register(id)
			if !hc.send(f) {
				sendBack(&protocol.Frame{Kind: protocol.KindError, Body: mustMarshal(protocol.Error{
					ID: id, Reason: "host connection closed",
				})})
				continue
			}
			go func() {
				for rf := range replyCh {
					sendBack(rf)
					if rf.Kind == protocol.KindResult || rf.Kind == protocol.KindError {
						break
					}
				}
			}()
		case protocol.KindStdin, protocol.KindEndStdin, protocol.KindCancel:
			// Control frames for an existing request: forward and forget.
			hc.send(f)
		default:
			log.Printf("ignoring client frame kind=%q", f.Kind)
		}
	}
	close(writeCh)
	_ = sendBack // referenced above
}
