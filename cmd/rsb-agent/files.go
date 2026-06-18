// File operations for rsb-agent: stat / get / put. These reuse the same
// length-delimited protocol as exec, so a file transfer rides the SAME SSH
// connection as commands — zero new ports, zero new auth.
//
// Design notes:
//   - file_stat: one-shot, reply with FileStat (size/mtime/mode/exists). sync
//     uses this to decide whether to transfer.
//   - file_get: agent streams FileChunk frames back, last one has Done + Sha256.
//   - file_put: client streams FileChunk frames in; agent writes to a temp file
//     then atomic rename (AtomicPut), verifies Sha256 if provided.
//
// All paths are resolved against the request's Cwd (or session cwd). No shell.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"rsb/internal/protocol"
)

// chunkSize is the max payload per FileChunk frame. 64 KiB balances frame
// overhead vs memory; a 10 MiB file is ~160 frames.
const chunkSize = 64 * 1024

// resolvePath turns a request path into an absolute path on the agent host,
// honoring Cwd (request's or session's). Relative paths are joined to Cwd.
func (a *agent) resolvePath(req protocol.Request) (string, *session, error) {
	sess := a.resolveSession(req)
	base := req.Cwd
	if base == "" {
		base = sess.cwd
	}
	p := req.Path
	if p == "" {
		return "", sess, errors.New("empty path")
	}
	if !filepath.IsAbs(p) {
		if base == "" {
			var err error
			base, err = os.Getwd()
			if err != nil {
				return "", sess, err
			}
		}
		p = filepath.Join(base, p)
	}
	return p, sess, nil
}

// handleFileStat replies with the remote file's metadata. Used by sync.
func (a *agent) handleFileStat(req protocol.Request) {
	p, _, err := a.resolvePath(req)
	if err != nil {
		a.writeFrame(protocol.KindError, protocol.Error{ID: req.ID, Reason: err.Error()})
		return
	}
	st, err := os.Stat(p)
	stat := protocol.FileStat{ID: req.ID}
	if err == nil {
		stat.Exists = true
		stat.Size = st.Size()
		stat.ModTime = st.ModTime().Unix()
		stat.Mode = uint32(st.Mode())
		stat.IsDir = st.IsDir()
	}
	// Non-existent is NOT an error for stat — sync needs to know it's absent.
	a.writeFrame(protocol.KindFileStat, stat)
}

// handleFileGet streams a remote file back to the client as FileChunk frames.
// Runs in its own goroutine so it doesn't block the read loop.
func (a *agent) handleFileGet(req protocol.Request) {
	a.pending.Add(1)
	defer a.pending.Done()

	p, _, err := a.resolvePath(req)
	if err != nil {
		a.writeFrame(protocol.KindError, protocol.Error{ID: req.ID, Reason: err.Error()})
		return
	}
	f, err := os.Open(p)
	if err != nil {
		a.writeFrame(protocol.KindError, protocol.Error{ID: req.ID, Reason: "open: " + err.Error()})
		return
	}
	defer f.Close()

	h := sha256.New()
	truncated := false

	switch {
	// Line-range read: use a scanner to emit only the requested lines. This
	// is the "agent reads the head of a big log" path — huge bandwidth saver
	// vs. transferring the whole file then trimming client-side.
	case req.LineStart > 0 || req.LineEnd > 0:
		start := req.LineStart
		if start == 0 {
			start = 1
		}
		end := req.LineEnd // 0 = unlimited
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024) // 1MB max line
		lineNo := 0
		var total int64
		for scanner.Scan() {
			lineNo++
			if lineNo < start {
				continue
			}
			if end > 0 && lineNo > end {
				// Reached the requested line end — NOT truncation. We read
				// exactly what was asked; only a MaxBytes cutoff mid-read counts.
				break
			}
			line := scanner.Bytes()
			// Add newline (Scanner strips it). Preserve original line endings
			// is overkill for the log-reading use case; normalize to \n.
			out := make([]byte, 0, len(line)+1)
			out = append(out, line...)
			out = append(out, '\n')
			// Honor MaxBytes even in line mode.
			if req.MaxBytes > 0 && total+int64(len(out)) > req.MaxBytes {
				truncated = true
				break
			}
			total += int64(len(out))
			h.Write(out)
			a.writeFrame(protocol.KindFileChunk, protocol.FileChunk{ID: req.ID, Data: out})
		}
		if err := scanner.Err(); err != nil {
			a.writeFrame(protocol.KindError, protocol.Error{ID: req.ID, Reason: "scan: " + err.Error()})
			return
		}

	// Whole-file read with optional byte cap. The default path; MaxBytes
	// protects against OOM on huge files (learned from Warp's byte budget).
	default:
		buf := make([]byte, chunkSize)
		var total int64
		for {
			// If we're under a byte cap, shrink the next read to not overshoot.
			toRead := len(buf)
			if req.MaxBytes > 0 {
				remaining := req.MaxBytes - total
				if remaining <= 0 {
					truncated = true
					break
				}
				if remaining < int64(toRead) {
					toRead = int(remaining)
				}
			}
			n, rerr := f.Read(buf[:toRead])
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				h.Write(chunk)
				total += int64(n)
				a.writeFrame(protocol.KindFileChunk, protocol.FileChunk{ID: req.ID, Data: chunk})
			}
			if rerr != nil {
				if !errors.Is(rerr, io.EOF) {
					a.writeFrame(protocol.KindError, protocol.Error{ID: req.ID, Reason: "read: " + rerr.Error()})
					return
				}
				break
			}
		}
	}

	// Final chunk: Done + Truncated flag + sha256 of what we actually sent.
	a.writeFrame(protocol.KindFileChunk, protocol.FileChunk{
		ID: req.ID, Done: true, Truncated: truncated,
		Sha256: hex.EncodeToString(h.Sum(nil)),
	})
	a.writeFrame(protocol.KindResult, protocol.Result{ID: req.ID, ExitCode: 0})
}

// handleFilePutSync receives a file upload by reading FileChunk frames
// DIRECTLY from stdin. It runs synchronously in the main read loop (not a
// goroutine) to provide natural backpressure: read one chunk, write it to
// disk, read the next. This avoids the deadlock that an async channel would
// cause — the channel buffer would fill against the read loop.
//
// No other request is serviced during a put; that's the right tradeoff for
// cp/sync, which are exclusive file operations.
func (a *agent) handleFilePutSync(req protocol.Request, stdin io.Reader) {
	p, _, err := a.resolvePath(req)
	if err != nil {
		a.writeFrame(protocol.KindError, protocol.Error{ID: req.ID, Reason: err.Error()})
		return
	}
	// Write target: temp file if atomic, else the path directly.
	target := p
	tmpPath := ""
	if req.AtomicPut {
		tmpPath = p + ".rsbtmp"
		target = tmpPath
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		a.writeFrame(protocol.KindError, protocol.Error{ID: req.ID, Reason: "mkdir: " + err.Error()})
		return
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(req.Mode))
	if err != nil {
		a.writeFrame(protocol.KindError, protocol.Error{ID: req.ID, Reason: "create: " + err.Error()})
		return
	}

	h := sha256.New()
	mw := io.MultiWriter(out, h)
	gotFinal := false
	var clientSha string
	// Read FileChunk frames from stdin until we see Done. Synchronous = the
	// main loop's natural read pace backpressures the client.
	for !gotFinal {
		f, rerr := protocol.ReadFrame(stdin)
		if rerr != nil {
			out.Close()
			os.Remove(target)
			a.writeFrame(protocol.KindError, protocol.Error{ID: req.ID, Reason: "stream read: " + rerr.Error()})
			return
		}
		if f.Kind != protocol.KindFileChunk {
			// A non-chunk frame mid-stream is unexpected; log and skip. (The
			// client shouldn't send anything but chunks after the put request.)
			log.Printf("file_put: unexpected frame kind=%q, ignoring", f.Kind)
			continue
		}
		var chunk protocol.FileChunk
		if err := json.Unmarshal(f.Body, &chunk); err != nil {
			continue
		}
		if len(chunk.Data) > 0 {
			if _, werr := mw.Write(chunk.Data); werr != nil {
				out.Close()
				os.Remove(target)
				a.writeFrame(protocol.KindError, protocol.Error{ID: req.ID, Reason: "write: " + werr.Error()})
				return
			}
		}
		if chunk.Done {
			gotFinal = true
			clientSha = chunk.Sha256
		}
	}
	out.Close()

	// Verify integrity if the client sent a checksum.
	if clientSha != "" && hex.EncodeToString(h.Sum(nil)) != clientSha {
		os.Remove(target)
		a.writeFrame(protocol.KindError, protocol.Error{ID: req.ID, Reason: "sha256 mismatch"})
		return
	}
	// Atomic rename into place.
	if req.AtomicPut {
		if err := os.Rename(tmpPath, p); err != nil {
			os.Remove(tmpPath)
			a.writeFrame(protocol.KindError, protocol.Error{ID: req.ID, Reason: "rename: " + err.Error()})
			return
		}
	}
	// Set mtime to match the source so sync's mtime+size check skips next time.
	if req.Mtime > 0 {
		t := time.Unix(req.Mtime, 0)
		_ = os.Chtimes(p, t, t)
	}
	a.writeFrame(protocol.KindResult, protocol.Result{ID: req.ID, ExitCode: 0})
}
