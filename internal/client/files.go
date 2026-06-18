// File transfer for the rsb client: rsb cp and rsb sync.
//
// These ride the SAME daemon connection as exec — no new ports, no scp, no
// path-escaping. File content travels as FileChunk frames over the existing
// SSH channel. The big payoff over scp: paths with spaces/quotes are just
// argv strings, not shell-escaped nightmares.
//
// Layout of this file:
//   - session: a reusable daemon connection for issuing requests + reading
//     streaming replies. cp/sync issue many small requests, so we open one
//     session per cp/sync invocation rather than one per file.
//   - fileStat / fileGet / filePut: thin wrappers that send one request and
//     collect its reply (or stream).
//   - CP: single-file upload or download.
//   - Sync: directory walk, mtime+size compare, selective upload.
package client

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"rsb/internal/protocol"
)

// session is one daemon connection, bound to a host. It lets cp/sync issue
// many requests without reconnecting each time.
type session struct {
	conn net.Conn
}

// newSession dials the daemon (auto-starting it), attaches to host, and drains
// the forwarded Hello. Returns a session ready for requests.
func newSession(host string) (*session, error) {
	conn, err := dialOrStart()
	if err != nil {
		return nil, err
	}
	if err := attach(conn, host); err != nil {
		conn.Close()
		return nil, err
	}
	// Drain Hello (or surface a handshake error).
	hf, err := protocol.ReadFrame(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("connect to daemon: %w", err)
	}
	if hf.Kind == protocol.KindError {
		var e protocol.Error
		json.Unmarshal(hf.Body, &e)
		conn.Close()
		return nil, errors.New(e.Reason)
	}
	return &session{conn: conn}, nil
}

func (s *session) close() { s.conn.Close() }

// send writes one request frame.
func (s *session) send(req *protocol.Request) error {
	return protocol.WriteFrame(s.conn, protocol.KindRequest, req)
}

// sendChunk writes one FileChunk frame (for file_put uploads).
func (s *session) sendChunk(c protocol.FileChunk) error {
	return protocol.WriteFrame(s.conn, protocol.KindFileChunk, c)
}

// readFrame reads the next frame (blocking).
func (s *session) readFrame() (*protocol.Frame, error) {
	return protocol.ReadFrame(s.conn)
}

// ---- file stat ----

// FileStat queries a remote file's metadata.
func (s *session) FileStat(remotePath, cwd string) (*protocol.FileStat, error) {
	id := fileID()
	if err := s.send(&protocol.Request{ID: id, Type: "file_stat", Path: remotePath, Cwd: cwd}); err != nil {
		return nil, err
	}
	for {
		f, err := s.readFrame()
		if err != nil {
			return nil, err
		}
		switch f.Kind {
		case protocol.KindFileStat:
			var st protocol.FileStat
			if err := json.Unmarshal(f.Body, &st); err == nil {
				return &st, nil
			}
		case protocol.KindError:
			var e protocol.Error
			json.Unmarshal(f.Body, &e)
			return nil, errors.New(e.Reason)
		}
	}
}

// ---- file get (download: remote -> local) ----

// FileGet downloads a remote file, writing content to w. Returns the sha256
// the agent reported (empty if the agent didn't send one).
func (s *session) FileGet(remotePath, cwd string, w io.Writer) (string, error) {
	id := fileID()
	if err := s.send(&protocol.Request{ID: id, Type: "file_get", Path: remotePath, Cwd: cwd}); err != nil {
		return "", err
	}
	h := sha256.New()
	mw := io.MultiWriter(w, h)
	var agentSha string
	for {
		f, err := s.readFrame()
		if err != nil {
			return "", err
		}
		switch f.Kind {
		case protocol.KindFileChunk:
			var c protocol.FileChunk
			if err := json.Unmarshal(f.Body, &c); err != nil {
				continue
			}
			if len(c.Data) > 0 {
				mw.Write(c.Data)
			}
			if c.Done {
				agentSha = c.Sha256
			}
		case protocol.KindResult:
			// Transfer complete.
			localSha := hex.EncodeToString(h.Sum(nil))
			if agentSha != "" && agentSha != localSha {
				return "", fmt.Errorf("sha256 mismatch on download: agent %s != local %s", agentSha, localSha)
			}
			return agentSha, nil
		case protocol.KindError:
			var e protocol.Error
			json.Unmarshal(f.Body, &e)
			return "", errors.New(e.Reason)
		}
	}
}

// ---- file put (upload: local -> remote) ----

// FilePut uploads a local file to a remote path, atomically if requested.
// mtime (unix seconds, 0 to skip) is set on the remote file after write so
// sync's mtime+size check skips it next time. Reads from the file, streams in
// chunks, sends sha256 + done.
func (s *session) FilePut(localPath, remotePath, cwd string, mode os.FileMode, atomic bool, mtime int64) error {
	id := fileID()
	req := &protocol.Request{
		ID:        id,
		Type:      "file_put",
		Path:      remotePath,
		Cwd:       cwd,
		Mode:      int(mode),
		Mtime:     mtime,
		AtomicPut: atomic,
	}
	if err := s.send(req); err != nil {
		return err
	}
	// Stream the file content as chunks. Compute sha256 as we go.
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 64*1024)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			h.Write(chunk)
			if err := s.sendChunk(protocol.FileChunk{ID: id, Data: chunk}); err != nil {
				return err
			}
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				return rerr
			}
			break
		}
	}
	// Final chunk: done + checksum.
	if err := s.sendChunk(protocol.FileChunk{
		ID: id, Done: true, Sha256: hex.EncodeToString(h.Sum(nil)),
	}); err != nil {
		return err
	}
	// Wait for the agent's Result/Error.
	for {
		f, err := s.readFrame()
		if err != nil {
			return err
		}
		switch f.Kind {
		case protocol.KindResult:
			return nil
		case protocol.KindError:
			var e protocol.Error
			json.Unmarshal(f.Body, &e)
			return errors.New(e.Reason)
		}
	}
}

// ---- rsb cp ----

// PathSpec is one side of a cp/sync argument: "host:path" or "local-path".
// The "local" host means this machine (no SSH).
type PathSpec struct {
	Host string // "" or "local" = local filesystem; otherwise an ssh host
	Path string
}

// ParsePath parses a cp argument like "prod:/opt/app/x.py" or "./local.py".
// A path with a colon has a host prefix; without a colon it's local.
// Edge case: Windows drive letters (C:\) — we treat a single-letter prefix
// followed by colon-backslash as local, not a host.
func ParsePath(s string) PathSpec {
	// "local:foo" is also accepted as local for clarity.
	if strings.HasPrefix(s, "local:") {
		return PathSpec{Host: "local", Path: s[len("local:"):]}
	}
	// Find the colon that separates host from path. But a leading single
	// letter + colon + backslash is a Windows path, not a host.
	if i := strings.Index(s, ":"); i > 0 {
		if i == 1 && (len(s) > 2 && s[2] == '\\') {
			return PathSpec{Host: "", Path: s}
		}
		return PathSpec{Host: s[:i], Path: s[i+1:]}
	}
	return PathSpec{Host: "", Path: s}
}

// IsRemote reports whether this spec should go through the daemon (either a
// real SSH host, or the "local" pseudo-host which runs the agent on this
// machine). A spec with no host at all is a pure local filesystem path.
func (p PathSpec) IsRemote() bool { return p.Host != "" }

// CP copies a single file between local and remote (either direction).
//   - src remote, dst local: download
//   - src local, dst remote: upload (atomic)
//   - both local or both remote: not supported (use cp/scp directly)
//
// mode preserves the source file's permission bits on upload.
func CP(src, dst PathSpec) error {
	if src.IsRemote() == dst.IsRemote() {
		return fmt.Errorf("cp needs one local and one remote path (got %s %s)", src.Host, dst.Host)
	}
	// Download: remote -> local.
	if src.IsRemote() {
		sess, err := newSession(src.Host)
		if err != nil {
			return err
		}
		defer sess.close()
		out, err := os.Create(dst.Path)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = sess.FileGet(src.Path, "", out)
		return err
	}
	// Upload: local -> remote.
	st, err := os.Stat(src.Path)
	if err != nil {
		return err
	}
	if st.IsDir() {
		return fmt.Errorf("%s is a directory; use rsb sync for directories", src.Path)
	}
	sess, err := newSession(dst.Host)
	if err != nil {
		return err
	}
	defer sess.close()
	return sess.FilePut(src.Path, dst.Path, "", st.Mode(), true, st.ModTime().Unix())
}

// ---- rsb sync ----

// SyncResult reports what sync did.
type SyncResult struct {
	Uploaded []string
	Skipped  []string
	Failed   map[string]error
}

// Sync uploads local files under srcDir to dst (a remote dir), transferring
// only files whose mtime or size differs from the remote copy (mtime+size
// heuristic, like rsync's default). Respects dryRun (lists what would
// transfer without writing).
func Sync(srcDir string, dst PathSpec, dryRun bool) (*SyncResult, error) {
	if !dst.IsRemote() {
		return nil, fmt.Errorf("sync destination must be remote (host:path)")
	}
	res := &SyncResult{Failed: map[string]error{}}
	sess, err := newSession(dst.Host)
	if err != nil {
		return nil, err
	}
	defer sess.close()

	// Walk the local source dir. For each file, decide transfer vs skip.
	err = filepath.Walk(srcDir, func(localPath string, info os.FileInfo, err error) error {
		if err != nil {
			res.Failed[localPath] = err
			return nil
		}
		if info.IsDir() {
			return nil
		}
		// Remote path = dst.Path + relative path.
		rel, _ := filepath.Rel(srcDir, localPath)
		remotePath := filepath.Join(dst.Path, filepath.ToSlash(rel))

		// Compare against remote stat.
		rstat, statErr := sess.FileStat(remotePath, "")
		needUpload := true
		if statErr == nil && rstat != nil && rstat.Exists && !rstat.IsDir {
			// mtime+size heuristic: skip if both match.
			if rstat.Size == info.Size() && rstat.ModTime == info.ModTime().Unix() {
				needUpload = false
			}
		}
		if !needUpload {
			res.Skipped = append(res.Skipped, rel)
			return nil
		}
		if dryRun {
			res.Uploaded = append(res.Uploaded, rel+" (dry-run)")
			return nil
		}
		if err := sess.FilePut(localPath, remotePath, "", info.Mode(), true, info.ModTime().Unix()); err != nil {
			res.Failed[rel] = err
			return nil
		}
		// Set remote mtime to match local so the next sync skips it.
		res.Uploaded = append(res.Uploaded, rel)
		return nil
	})
	if err != nil {
		return res, err
	}
	return res, nil
}

// _ keeps imports honest if some are only used under build tags later.
var _ = time.Second

// fileID generates a process-unique request id for file operations.
var fileCounter uint64

func fileID() string {
	n := fileCounter + 1
	fileCounter++
	return fmt.Sprintf("file-%d-%d", time.Now().UnixNano(), n)
}
