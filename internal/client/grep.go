// rsb grep: search remote files with ripgrep, streaming structured matches
// back. The bandwidth win over `rsb exec host -- grep -rn pattern` is that rg
// runs on the REMOTE host (using its CPU + gitignore awareness) and we only
// carry the matched lines back, not entire files. And we parse rg's --json
// output so the result is structured (file:line:content), not raw text.
//
// Zero protocol change: this is a client-side composition over `rsb exec`.
// We build `rg --json <flags> <pattern> <roots>` as argv, send it as a normal
// exec, and parse the streamed stdout line-by-line. If the remote lacks rg,
// the caller can fall back to grep -rn (less structured, but still works).
package client

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"rsb/internal/protocol"
)

// SearchMatch is one matched line from a remote search.
type SearchMatch struct {
	File   string // path relative to the search root
	Line   int    // 1-indexed line number
	Column int    // 1-indexed column of first match (0 if unknown)
	Text   string // the matched line's text (trimmed of trailing newline)
}

// SearchOptions controls a remote grep.
type SearchOptions struct {
	IgnoreCase bool     // -i: case-insensitive
	WordRegex  bool     // -w: match whole words only
	Fixed      bool     // -F: literal string, not regex
	MaxMatches int      // stop after N matches (rg -m applies per-file; we cap globally)
	Glob       string   // --glob: include pattern (e.g. "*.go")
	Roots      []string // directories to search (default: ".")
}

// Grep runs ripgrep on the remote host and returns structured matches. It
// streams rg's --json output, parsing match entries into SearchMatch. The
// caller gets clean file:line:content triples without pulling whole files.
//
// If the remote has no rg, the error says so; the caller can retry with
// GrepFallback (grep -rn) which is less precise but universal.
func (s *Session) Grep(pattern string, opts SearchOptions, out io.Writer) (int, error) {
	// Build the rg argv. --json gives us structured output we can parse.
	argv := []string{"rg", "--json", "--line-number", "--with-filename"}
	if opts.IgnoreCase {
		argv = append(argv, "-i")
	}
	if opts.WordRegex {
		argv = append(argv, "-w")
	}
	if opts.Fixed {
		argv = append(argv, "-F")
	}
	if opts.Glob != "" {
		argv = append(argv, "--glob", opts.Glob)
	}
	if opts.MaxMatches > 0 {
		// -m is per-file in rg; we also cap globally while parsing.
		argv = append(argv, "-m", fmt.Sprintf("%d", opts.MaxMatches))
	}
	argv = append(argv, "--", pattern)
	roots := opts.Roots
	if len(roots) == 0 {
		roots = []string{"."}
	}
	argv = append(argv, roots...)

	// Send as a normal exec; the streamed stdout is rg's JSON lines. We use a
	// pipe to capture stdout while stderr goes to our caller's stderr via the
	// Exec helper — but here we need to parse stdout, so we drive the session
	// directly.
	req := &protocol.Request{
		ID:          fileID(),
		Type:        "exec",
		Argv:        argv,
		StdinClosed: true,
	}
	if err := s.send(req); err != nil {
		return 0, err
	}

	// Collect stdout (rg JSON) and stderr (rg errors / "rg: command not found")
	// separately by reading frames. We accumulate stdout into a parser and
	// tee stderr to out for diagnostics.
	var stderrBuf strings.Builder
	count := 0
	cap := opts.MaxMatches
	for {
		f, err := s.readFrame()
		if err != nil {
			return count, err
		}
		switch f.Kind {
		case protocol.KindOutput:
			var o protocol.Output
			if err := json.Unmarshal(f.Body, &o); err != nil {
				continue
			}
			if o.Stream == "stdout" {
				// Parse rg --json lines from this chunk.
				count += parseRgJSON(o.Data, out, cap-count)
			} else {
				stderrBuf.Write(o.Data)
			}
		case protocol.KindResult:
			// rg ran and exited. stderr may contain non-fatal warnings (e.g.
			// permission denied on some files); we DON'T treat those as "rg
			// missing". The agent's exec.LookPath failure arrives as a
			// KindError frame (handled below), so by the time we reach Result,
			// rg definitely existed.
			if stderrBuf.Len() > 0 {
				// Surface non-empty stderr only if there were zero matches, so
				// the user knows why nothing came back (e.g. all dirs skipped).
				if count == 0 {
					fmt.Fprintf(out, "rsb: (rg stderr) %s", stderrBuf.String())
				}
			}
			return count, nil
		case protocol.KindError:
			var e protocol.Error
			json.Unmarshal(f.Body, &e)
			// The agent sends "executable not found in PATH: rg" when rg is
			// absent. Translate to an actionable hint.
			if strings.Contains(e.Reason, "not found in PATH") {
				return count, fmt.Errorf("ripgrep (rg) not found on remote host\n"+
					"install: ssh %s 'sudo apt install ripgrep'  # or: brew/brew install ripgrep\n"+
					"fallback: rsb exec <host> -- grep -rn %q %v",
					"(host)", pattern, roots)
			}
			return count, fmt.Errorf("%s", e.Reason)
		}
		// Honor the global match cap.
		if cap > 0 && count >= cap {
			break
		}
	}
	return count, nil
}

// parseRgJSON parses a chunk of rg --json output, printing human-readable
// match lines (file:line: content) and returning the number of matches found.
// rg emits one JSON object per line; a chunk may contain partial lines, so we
// buffer across calls using a line splitter.
//
// We track a per-call buffer via a closure-scoped scanner. Since Go doesn't
// give us easy stateful parsers here, we use a package-level buffer. (This is
// acceptable because grep sessions are single-threaded per connection.)
var rgLineBuf strings.Builder

func parseRgJSON(chunk []byte, out io.Writer, remaining int) int {
	rgLineBuf.Write(chunk)
	count := 0
	sc := bufio.NewScanner(strings.NewReader(rgLineBuf.String()))
	sc.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	var complete strings.Builder
	for sc.Scan() {
		line := sc.Bytes()
		// Try to parse as a rg JSON object.
		var entry struct {
			Type string `json:"type"`
			Data struct {
				Path struct {
					Text string `json:"text"`
				} `json:"path"`
				Lines struct {
					Text string `json:"text"`
				} `json:"lines"`
				LineNumber int `json:"line_number"`
				Submatches []struct {
					Start int `json:"start"`
				} `json:"submatches"`
			} `json:"data"`
		}
		if json.Unmarshal(line, &entry) != nil {
			// Not valid JSON — might be a partial line; keep for next chunk.
			complete.Write(line)
			continue
		}
		if entry.Type == "match" {
			text := strings.TrimRight(entry.Data.Lines.Text, "\n")
			col := 0
			if len(entry.Data.Submatches) > 0 {
				col = entry.Data.Submatches[0].Start + 1
			}
			fmt.Fprintf(out, "%s:%d:%s\n", entry.Data.Path.Text, entry.Data.LineNumber, text)
			_ = col // available for callers who want column; omitted in default output for brevity
			count++
			if remaining > 0 && count >= remaining {
				rgLineBuf.Reset()
				return count
			}
		}
	}
	// Keep any trailing partial line for the next chunk.
	rgLineBuf.Reset()
	rgLineBuf.WriteString(complete.String())
	return count
}
