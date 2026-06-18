// Command rsb is the local client for rsb (remote-shell-bridge).
//
// P1 architecture: rsb exec talks to a local daemon (auto-started if absent),
// which keeps one SSH connection per host and multiplexes requests. This
// gives connection reuse and session/cwd persistence across invocations.
//
// Subcommands:
//
//	rsb exec <host> --argv '<json>' [--cwd DIR] [--env K=V]... [--timeout MS] [--session NAME]
//	rsb exec --local --argv '<json>' [...]        # same, but on this machine
//	rsb daemon [start|stop|status]                # manage the local daemon
//	rsb ensure <host>                             # install rsb-agent remotely
//	rsb version
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"rsb/internal/client"
	"rsb/internal/paths"
	"rsb/internal/protocol"
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	// Top-level help. Recognize -h/--help/help anywhere as the first arg, and
	// also accept them after a subcommand (e.g. `rsb exec --help`).
	switch os.Args[1] {
	case "-h", "--help", "help":
		usage(os.Stdout)
		return
	case "exec":
		if wantsHelp(os.Args[2:]) {
			helpExec()
			return
		}
		cmdExec(os.Args[2:])
	case "cp":
		if wantsHelp(os.Args[2:]) {
			helpCp()
			return
		}
		cmdCp(os.Args[2:])
	case "sync":
		if wantsHelp(os.Args[2:]) {
			helpSync()
			return
		}
		cmdSync(os.Args[2:])
	case "repl":
		if wantsHelp(os.Args[2:]) {
			helpRepl()
			return
		}
		cmdRepl(os.Args[2:])
	case "daemon":
		if wantsHelp(os.Args[2:]) {
			helpDaemon()
			return
		}
		cmdDaemon(os.Args[2:])
	case "ensure":
		if wantsHelp(os.Args[2:]) {
			helpEnsure()
			return
		}
		cmdEnsure(os.Args[2:])
	case "doctor":
		if wantsHelp(os.Args[2:]) {
			helpDoctor()
			return
		}
		cmdDoctor(os.Args[2:])
	case "install-local":
		if wantsHelp(os.Args[2:]) {
			helpInstallLocal()
			return
		}
		cmdInstallLocal(os.Args[2:])
	case "agent-version":
		if wantsHelp(os.Args[2:]) {
			helpAgentVersion()
			return
		}
		cmdAgentVersion(os.Args[2:])
	case "cat":
		if wantsHelp(os.Args[2:]) {
			helpCat()
			return
		}
		cmdCat(os.Args[2:])
	case "grep":
		if wantsHelp(os.Args[2:]) {
			helpGrep()
			return
		}
		cmdGrep(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Printf("rsb %s (protocol v%d)\n", version, protocol.ProtocolVersion)
	default:
		fmt.Fprintf(os.Stderr, "rsb: unknown command %q\n\n", os.Args[1])
		usage(os.Stderr)
		os.Exit(2)
	}
}

// wantsHelp reports whether args contains a help flag (-h/--help/help).
func wantsHelp(args []string) bool {
	for _, a := range args {
		switch a {
		case "-h", "--help", "help":
			return true
		}
	}
	return false
}

// version is the rsb client version. Overridden at build time via ldflags
// (-ldflags "-X main.version=..."); defaults to a dev marker.
var version = "0.3.0-dev"

// usage prints the top-level command summary. w is stdout for explicit help,
// stderr for misuse (callers follow with a non-zero exit).
func usage(w io.Writer) {
	fmt.Fprint(w, `rsb `+version+` - remote shell bridge for agents

Run commands on remote hosts or containers WITHOUT quoting hell: argv travels
as a JSON array to execve on the target, never through a shell.

USAGE
  rsb <command> [options]

COMMANDS
  exec <host> --argv '<json>'   Execute a command (argv array, no shell)
  exec <host> -- cmd args       ...or the ergonomic -- shorthand
  cp <src> <dst>                Copy a file local<->remote (host:path)
  cat <host:path>               Read a remote file (or slice) to stdout
  grep <host> <pattern> [paths] Search remote files with ripgrep
  sync <dir> <host:dir>         Incrementally upload a directory
  repl <host>                   Interactive multi-command session
  ensure <host> [--force]       Install/upgrade rsb-agent on a remote host
  agent-version <host>          Show the agent version installed on a host
  doctor [host]                 Self-check: home/binaries/daemon/ssh/docker
  install-local                 Create bin/ symlinks for the current platform
  daemon status|stop            Manage the local daemon (usually automatic)
  version                       Print version

GLOBAL OPTIONS
  -h, --help                    Show help (use `+"`"+`rsb <command> --help`+"`"+` for per-command)

QUICK START
  rsb install-local                       # one-time: set up bin/ symlinks
  rsb doctor                              # verify everything works
  rsb exec --local --argv '["echo","hi"]' # run locally (no ssh)
  rsb ensure prod-host                    # install agent on a remote host
  rsb exec prod-host --argv '["ls","-la"]'

EXAMPLE (the quoting-hell killer)
  rsb exec prod --argv '["echo","he said \"hi\" and $HOME"]'
  # argv is an array, so quotes/$ never get re-parsed by a shell

MORE
  rsb <command> --help    detailed options for each command
  See skill/SKILL.md for the full agent guide.
`)
}

// helpExec prints detailed help for `rsb exec`.
func helpExec() {
	fmt.Print(`rsb exec - run a command on a host or in a container

USAGE
  rsb exec <host> --argv '<json>' [options]
  rsb exec --local --argv '<json>' [options]

OPTIONS
  --argv '<json>'     (required) JSON string array, e.g. '["ls","-la"]'
  --cwd DIR           Working directory on the target
  --env K=V           Environment variable (repeatable); values are NOT expanded
  --timeout MS        Kill the command after N milliseconds
  --session NAME      Share cwd/env across commands; "cd" persists per session
  --container NAME    Run inside a Docker container (argv reaches it verbatim)
  --stdin             Pipe local stdin to the remote command
  --local             Run on this machine (no SSH); host is "local"

EXIT CODE
  rsb's exit code equals the remote command's exit code.

EXAMPLES
  rsb exec prod --argv '["grep","-rEn","TODO|FIXME","src/"]'
  rsb exec prod --session work --argv '["cd","/app"]'
  rsb exec prod --session work --argv '["pwd"]'            # still /app
  rsb exec prod --container api --argv '["env"]'
  cat file | rsb exec prod --stdin --argv '["docker","exec","-i","c","cat"]'
`)
}

func helpRepl() {
	fmt.Print(`rsb repl - interactive multi-command session (cwd persists)

USAGE
  rsb repl <host> [--session NAME]
  rsb repl --local [--session NAME]

INPUT FORMATS (one per line)
  ["argv","as","json"]     explicit argv array (safe for any quoting)
  some shell command       shorthand: wrapped as ["sh","-c","..."]

IN-SESSION COMMANDS
  :session NAME            switch/rename the active session
  :container NAME          run subsequent commands in a container
  :container               clear container (run on host again)
  :quit                    exit

EXAMPLE
  rsb repl prod --session work
  [prod:work] ["cd","/opt/app"]
  [prod:work] ["ls"]
  [prod:work] :container api
  [prod:work/api] ["env"]
  [prod:work/api] :quit
`)
}

func helpEnsure() {
	fmt.Print(`rsb ensure - install rsb-agent on a remote host

USAGE
  rsb ensure <host>

Probes the remote OS/arch via ssh, then scp's the matching rsb-agent binary
(located via the install home, not the cwd) to ~/.rsb/rsb-agent on the host.
Run once per host before `+"`"+`rsb exec <host>`+"`"+`.

EXAMPLE
  rsb ensure ubuntu@1.2.3.4
`)
}

func helpDoctor() {
	fmt.Print(`rsb doctor - self-check the rsb installation

USAGE
  rsb doctor [host] [--container=NAME]

CHECKS
  install home              Where rsb finds its binaries
  rsb/rsb-daemon/rsb-agent  Current-platform binaries present
  daemon                    Running? (auto-starts on first exec otherwise)
  ssh <host>                (if host given) reachable non-interactively?
  remote agent version      (if host given) actual --version output on host
  remote agent hash         (if host given) sha256 vs local — catches stale installs
  docker                    Daemon reachable; local container mode
  container exec <NAME>     (if --container given) REAL docker exec smoke test

The --container test actually runs `+"`"+`docker exec <NAME> true`+"`"+`, so a passing
check means `+"`"+`--container`+"`"+` will work, not just that docker is up.

Exits non-zero if any required check fails.
`)
}

func helpDaemon() {
	fmt.Print(`rsb daemon - manage the local rsb daemon

USAGE
  rsb daemon status       Show daemon pid and socket
  rsb daemon stop         Stop the daemon
  rsb daemon start        (no-op; auto-starts on first exec)

The daemon is normally fully automatic: the first `+"`"+`rsb exec`+"`"+` starts it
and it keeps SSH connections open for reuse. You rarely need these commands.
`)
}

func helpInstallLocal() {
	fmt.Print(`rsb install-local - create convenient bin/ symlinks

USAGE
  rsb install-local

Creates <home>/bin/rsb, <home>/bin/rsb-daemon, <home>/bin/rsb-agent as
symlinks to the current platform's binaries, so you can add <home>/bin to
PATH and call `+"`"+`rsb`+"`"+` directly instead of the platform-specific path.

AFTER RUNNING
  export PATH=<home>/bin:$PATH
  rsb version
`)
}

func helpAgentVersion() {
	fmt.Print(`rsb agent-version - show the version of the agent installed on a host

USAGE
  rsb agent-version <host>
  rsb agent-version --local

Reports the remote rsb-agent's version, protocol version, and build time by
running `+"`"+`rsb-agent --version`+"`"+` over ssh. Use this to verify a host actually runs
the agent you think it does (and match it against `+"`"+`rsb version`+"`"+` locally).

EXAMPLE
  rsb version                  # local client version
  rsb agent-version prod       # remote agent version — should match
`)
}

// cmdCp: rsb cp <src> <dst> — copy a single file between local and remote.
//   rsb cp local.py prod:/opt/app/x.py       (upload)
//   rsb cp prod:/var/log/app.log ./app.log   (download)
func cmdCp(args []string) {
	if len(args) < 2 {
		fatalf("usage: rsb cp <src> <dst>\n  e.g. rsb cp local.py prod:/opt/app/x.py")
	}
	src := client.ParsePath(args[0])
	dst := client.ParsePath(args[1])
	if err := client.CP(src, dst); err != nil {
		fatalf("cp: %v", err)
	}
	fmt.Fprintf(os.Stderr, "rsb: %s -> %s ok\n", args[0], args[1])
}

// cmdSync: rsb sync <localDir> <host:remoteDir> [--dry-run]
// Uploads local files to remote, transferring only those whose mtime/size
// differ (rsync-style heuristic).
func cmdSync(args []string) {
	dryRun := false
	var positional []string
	for _, a := range args {
		if a == "--dry-run" {
			dryRun = true
		} else {
			positional = append(positional, a)
		}
	}
	if len(positional) < 2 {
		fatalf("usage: rsb sync <localDir> <host:remoteDir> [--dry-run]")
	}
	srcDir := positional[0]
	dst := client.ParsePath(positional[1])
	res, err := client.Sync(srcDir, dst, dryRun)
	if err != nil {
		fatalf("sync: %v", err)
	}
	for _, f := range res.Uploaded {
		fmt.Fprintf(os.Stderr, "  upload  %s\n", f)
	}
	for _, f := range res.Skipped {
		fmt.Fprintf(os.Stderr, "  skip    %s\n", f)
	}
	for f, e := range res.Failed {
		fmt.Fprintf(os.Stderr, "  FAIL    %s: %v\n", f, e)
	}
	summary := fmt.Sprintf("%d uploaded, %d skipped", len(res.Uploaded), len(res.Skipped))
	if len(res.Failed) > 0 {
		summary += fmt.Sprintf(", %d failed", len(res.Failed))
	}
	if dryRun {
		summary += " (dry-run)"
	}
	fmt.Fprintf(os.Stderr, "rsb: %s\n", summary)
	if len(res.Failed) > 0 {
		os.Exit(1)
	}
}

// cmdCat: rsb cat <host:path> [--lines N[:M]] [--max-bytes N]
// Reads a remote file (or a slice of it) and prints to stdout. The line range
// and byte cap are enforced SERVER-SIDE, so only the requested bytes travel
// over the wire — a huge bandwidth saver vs. cp-then-trim. This is rsb's
// answer to Warp's ReadFileContext.
func cmdCat(args []string) {
	var linesFlag, maxBytesFlag string
	var positional []string
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "--lines="):
			linesFlag = strings.TrimPrefix(a, "--lines=")
		case strings.HasPrefix(a, "--max-bytes="):
			maxBytesFlag = strings.TrimPrefix(a, "--max-bytes=")
		default:
			positional = append(positional, a)
		}
	}
	if len(positional) < 1 {
		fatalf("usage: rsb cat <host:path> [--lines N[:M]] [--max-bytes N]")
	}
	spec := client.ParsePath(positional[0])
	if !spec.IsRemote() {
		// Local: just read the file directly (no daemon round-trip needed).
		f, err := os.Open(spec.Path)
		if err != nil {
			fatalf("%v", err)
		}
		defer f.Close()
		applyCatLimits(os.Stdout, f, linesFlag, maxBytesFlag)
		return
	}
	opts := client.FileGetOptions{}
	if linesFlag != "" {
		start, end := parseLineRange(linesFlag)
		opts.LineStart = start
		opts.LineEnd = end
	}
	if maxBytesFlag != "" {
		n, err := strconv.ParseInt(maxBytesFlag, 10, 64)
		if err != nil {
			fatalf("bad --max-bytes: %v", err)
		}
		opts.MaxBytes = n
	}
	sess, err := client.NewSession(spec.Host)
	if err != nil {
		fatalf("%v", err)
	}
	defer sess.Close()
	res, err := sess.FileGet(spec.Path, "", os.Stdout, opts)
	if err != nil {
		fatalf("%v", err)
	}
	if res.Truncated {
		fmt.Fprintf(os.Stderr, "rsb: (truncated — use --lines/--max-bytes to read more)\n")
	}
}

// parseLineRange parses "N" (line N only) or "N:M" (lines N to M inclusive).
func parseLineRange(s string) (start, end int) {
	if i := strings.IndexByte(s, ':'); i >= 0 {
		start, _ = strconv.Atoi(s[:i])
		end, _ = strconv.Atoi(s[i+1:])
		return start, end
	}
	n, _ := strconv.Atoi(s)
	return n, n
}

// applyCatLimits reads from r with optional line/byte limits, writing to w.
// Used for the local-file path of `rsb cat` (no daemon involved).
func applyCatLimits(w io.Writer, r io.Reader, linesFlag, maxBytesFlag string) {
	start, end := 0, 0
	if linesFlag != "" {
		start, end = parseLineRange(linesFlag)
	}
	var maxBytes int64
	if maxBytesFlag != "" {
		maxBytes, _ = strconv.ParseInt(maxBytesFlag, 10, 64)
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	lineNo := 0
	var total int64
	for sc.Scan() {
		lineNo++
		if start > 0 && lineNo < start {
			continue
		}
		if end > 0 && lineNo > end {
			break
		}
		line := sc.Bytes()
		out := append(line, '\n')
		if maxBytes > 0 && total+int64(len(out)) > maxBytes {
			fmt.Fprintf(os.Stderr, "rsb: (truncated at --max-bytes)\n")
			return
		}
		total += int64(len(out))
		w.Write(out)
	}
}

func helpGrep() {
	fmt.Print(`rsb grep - search remote files with ripgrep (structured, bandwidth-efficient)

USAGE
  rsb grep <host> [options] <pattern> [paths...]
  rsb grep --local [options] <pattern> [paths...]

Runs ripgrep on the REMOTE host, parsing its --json output into clean
file:line:content lines. Only matched lines travel back — not whole files.
Honors the remote's .gitignore.

OPTIONS
  -i, --ignore-case     case-insensitive
  -w, --word             match whole words only
  -F, --fixed            literal string (not regex)
  --glob PATTERN         include only matching files (e.g. "*.go")
  --max-matches N        stop after N hits
  --cwd DIR              search root if paths are relative (default: remote home)

EXAMPLES
  rsb grep prod 'TODO|FIXME' /opt/app/src
  rsb grep prod -i --glob '*.py' 'def main' /opt/app
  rsb grep prod --max-matches 20 'database' /opt/app/config
  rsb grep --local 'func.*Session' internal/

If the remote lacks rg, the error says so; fall back to:
  rsb exec <host> -- grep -rn 'pattern' /path
`)
}

// cmdGrep: rsb grep <host> [flags] <pattern> [paths...]
// Searches remote files with ripgrep, printing structured file:line:content.
func cmdGrep(args []string) {
	host := "local"
	rest := args
	if len(rest) > 0 && rest[0] == "--local" {
		rest = rest[1:]
	} else if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		host = rest[0]
		rest = rest[1:]
	}

	opts := client.SearchOptions{}
	var positional []string
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		// Support both --flag value and --flag=value forms.
		if eq := strings.IndexByte(a, '='); eq > 0 && strings.HasPrefix(a, "--") {
			key := a[:eq]
			val := a[eq+1:]
			switch key {
			case "--glob":
				opts.Glob = val
				continue
			case "--max-matches":
				opts.MaxMatches, _ = strconv.Atoi(val)
				continue
			}
		}
		switch a {
		case "-i", "--ignore-case":
			opts.IgnoreCase = true
		case "-w", "--word":
			opts.WordRegex = true
		case "-F", "--fixed":
			opts.Fixed = true
		case "--glob":
			i++
			if i < len(rest) {
				opts.Glob = rest[i]
			}
		case "--max-matches":
			i++
			if i < len(rest) {
				opts.MaxMatches, _ = strconv.Atoi(rest[i])
			}
		default:
			positional = append(positional, a)
		}
	}
	if len(positional) < 1 {
		fatalf("usage: rsb grep <host> [options] <pattern> [paths...]")
	}
	pattern := positional[0]
	opts.Roots = positional[1:]

	sess, err := client.NewSession(host)
	if err != nil {
		fatalf("%v", err)
	}
	defer sess.Close()
	count, err := sess.Grep(pattern, opts, os.Stdout)
	if err != nil {
		fatalf("%v", err)
	}
	if count == 0 {
		fmt.Fprintln(os.Stderr, "rsb: no matches")
	}
}

func helpCat() {
	fmt.Print(`rsb cat - read a remote file (or a slice of it) to stdout

USAGE
  rsb cat <host:path> [--lines N[:M]] [--max-bytes BYTES]
  rsb cat <local-path> [--lines N[:M]] [--max-bytes BYTES]

Line ranges and byte caps are enforced on the REMOTE side: only the requested
bytes travel over the wire. Ideal for peeking at the head of a large log or
reading a config section without downloading the whole file.

OPTIONS
  --lines N       read only line N (1-indexed)
  --lines N:M     read lines N through M (inclusive)
  --max-bytes N   stop after N bytes (prevents OOM on huge files)

EXAMPLES
  rsb cat prod:/var/log/app.log --lines 1:100      # first 100 lines
  rsb cat prod:/etc/nginx/nginx.conf --lines 50    # just line 50
  rsb cat prod:/var/log/app.log --max-bytes 4096   # first 4KB
`)
}

func helpCp() {
	fmt.Print(`rsb cp - copy a single file between local and remote

USAGE
  rsb cp <src> <dst>

One side must be local, the other remote (host:path). File content travels
over the rsb daemon connection — no scp, no path escaping.

EXAMPLES
  rsb cp ./service.py prod:/opt/app/service.py       # upload
  rsb cp prod:/var/log/app.log ./app.log             # download
  rsb cp ./config.json prod:config.json              # upload (relative to remote home)
`)
}

func helpSync() {
	fmt.Print(`rsb sync - upload a local directory to a remote host (incremental)

USAGE
  rsb sync <localDir> <host:remoteDir> [--dry-run]

Walks localDir and uploads each file to remoteDir, transferring only files
whose size or mtime differs from the remote copy (rsync-style heuristic).
Atomic writes + sha256 verification per file.

OPTIONS
  --dry-run    list what would transfer without writing

EXAMPLES
  rsb sync ./orchestrator prod:/home/ubuntu/app/orchestrator
  rsb sync ./orchestrator prod:/home/ubuntu/app/orchestrator --dry-run
`)
}

// cmdRepl: rsb repl <host> [--session NAME] [--local]
func cmdRepl(args []string) {
	host := "local"
	rest := args
	if len(rest) > 0 && rest[0] == "--local" {
		rest = rest[1:]
	} else if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		host = rest[0]
		rest = rest[1:]
	}
	var session string
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--session":
			i++
			if i < len(rest) {
				session = rest[i]
			}
		default:
			fatalf("unknown repl flag: %s", rest[i])
		}
	}
	os.Exit(client.Repl(host, session, os.Stdin, os.Stdout, os.Stderr))
}

// cmdExec: rsb exec <host> --argv '...' [flags]
func cmdExec(args []string) {
	host := "local"
	rest := args
	// Allow "--local" as a leading flag shorthand for host=local.
	if len(rest) > 0 && rest[0] == "--local" {
		rest = rest[1:]
	} else if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		host = rest[0]
		rest = rest[1:]
	}

	var (
		argvStr   string
		argvRest  []string // argv from the `--` shorthand
		cwd       string
		envFlags  multiFlag
		timeoutMs int
		session   string
		container string
		stdinFlag bool
	)
	// Two argv forms:
	//   --argv '<json>'   explicit JSON array (authoritative; safe for any quoting)
	//   -- a b c          shorthand: everything after `--` becomes argv verbatim.
	// The `--` form is the ergonomic one — no JSON, no quoting. The CLI builds
	// argv[] locally; it NEVER reaches a shell, so spaces/quotes in args are
	// preserved exactly the same way as the JSON form.
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		if a == "--" {
			argvRest = rest[i+1:]
			break
		}
		switch a {
		case "--argv":
			i++
			if i < len(rest) {
				argvStr = rest[i]
			}
		case "--cwd":
			i++
			if i < len(rest) {
				cwd = rest[i]
			}
		case "--env":
			i++
			if i < len(rest) {
				envFlags = append(envFlags, rest[i])
			}
		case "--timeout":
			i++
			if i < len(rest) {
				timeoutMs, _ = strconv.Atoi(rest[i])
			}
		case "--session":
			i++
			if i < len(rest) {
				session = rest[i]
			}
		case "--container":
			i++
			if i < len(rest) {
				container = rest[i]
			}
		case "--stdin":
			stdinFlag = true
		default:
			fatalf("unknown flag: %s (use -- to pass a raw command)", a)
		}
	}

	// Resolve argv from exactly one of the two forms.
	var argv []string
	switch {
	case argvStr != "" && len(argvRest) > 0:
		fatalf("specify either --argv '<json>' OR -- <cmd>, not both")
	case argvStr != "":
		if err := json.Unmarshal([]byte(argvStr), &argv); err != nil {
			fatalf("invalid --argv JSON: %v", err)
		}
	case len(argvRest) > 0:
		argv = argvRest
	default:
		fatalf("argv is required: rsb exec <host> --argv '<json>'   OR   rsb exec <host> -- cmd args...")
	}
	if len(argv) == 0 {
		fatalf("argv is empty")
	}
	envMap := map[string]string{}
	for _, kv := range envFlags {
		j := strings.IndexByte(kv, '=')
		if j < 0 {
			fatalf("bad --env %q (want K=V)", kv)
		}
		envMap[kv[:j]] = kv[j+1:]
	}

	req := &protocol.Request{
		ID:        newID(),
		Type:      "exec",
		Argv:      argv,
		Cwd:       cwd,
		Env:       envMap,
		TimeoutMs: timeoutMs,
		Session:   session,
		Container: container,
	}

	var stdin io.Reader
	if stdinFlag {
		stdin = os.Stdin
	}
	res, err := client.Exec(host, req, stdin, os.Stdout, os.Stderr)
	if err != nil {
		fatalf("%v", err)
	}
	os.Exit(res.ExitCode)
}

// cmdDaemon: rsb daemon [start|stop|status]
func cmdDaemon(args []string) {
	action := "status"
	if len(args) > 0 {
		action = args[0]
	}
	switch action {
	case "start":
		// Touching the daemon is normally automatic; explicit start is for
		// debugging. We just dial, which auto-starts if needed.
		if pid := readPID(); pid > 0 && processAlive(pid) {
			fmt.Fprintf(os.Stderr, "rsb: daemon already running pid=%d\n", pid)
			return
		}
		// Force a dial to bootstrap.
		fmt.Fprintln(os.Stderr, "rsb: (daemon auto-starts on next exec; no manual start needed)")
	case "stop":
		pid := readPID()
		if pid <= 0 {
			fmt.Fprintln(os.Stderr, "rsb: no daemon running")
			return
		}
		if err := killPID(pid); err != nil {
			fatalf("stop: %v", err)
		}
		os.Remove(paths.PIDFile())
		fmt.Fprintf(os.Stderr, "rsb: daemon pid=%d stopped\n", pid)
	case "status":
		pid := readPID()
		if pid > 0 && processAlive(pid) {
			sock := paths.SocketPath()
			fmt.Fprintf(os.Stderr, "rsb: daemon running pid=%d socket=%s\n", pid, sock)
		} else {
			fmt.Fprintln(os.Stderr, "rsb: daemon not running")
		}
	default:
		fatalf("unknown daemon action: %s (use start|stop|status)", action)
	}
}

// cmdEnsure: rsb ensure <host> [--force] — install a platform-matching
// rsb-agent to ~/.rsb/ on the host.
//
// Reliability contract (fixes P8/P12): the install is atomic AND verified.
//   - Upload to a temp path (~/.rsb/.rsb-agent.new), not the final path, so a
//     failed scp never leaves a half-written agent.
//   - chmod + atomic mv (rename) into place. rename is atomic on POSIX.
//   - SHA256 of local vs remote: must match or we FAIL loudly. This is the
//     guard against the "looks installed but it's stale" trap.
//
// Without --force, if the remote hash already matches we skip the upload
// entirely (idempotent). With --force we always re-upload and re-verify.
func cmdEnsure(args []string) {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		fatalf("usage: rsb ensure <host> [--force]")
	}
	host := args[0]
	force := false
	for _, a := range args[1:] {
		switch a {
		case "--force":
			force = true
		default:
			fatalf("unknown ensure flag: %s", a)
		}
	}

	remoteOS, remoteArch, err := probeRemoteArch(host)
	if err != nil {
		fatalf("probe %s: %v\nhint: ensure the host is reachable via ssh", host, err)
	}
	fmt.Fprintf(os.Stderr, "rsb: %s is %s/%s\n", host, remoteOS, remoteArch)

	localBin := paths.AgentForPlatform(remoteOS, remoteArch)
	if localBin == "" {
		home := paths.Home()
		fatalf("no rsb-agent for %s/%s found under rsb home %q\n"+
			"  expected: %s/bin/%s-%s/rsb-agent\n"+
			"fix: run from the rsb install dir, or set RSB_HOME, or rebuild:\n"+
			"  RSB_HOME=<path-to-rsb-skill> rsb ensure %s\n"+
			"  bash <home>/scripts/build.sh",
			remoteOS, remoteArch, home, home, remoteOS, remoteArch, host)
	}

	// Compute the local agent's sha256 up front; we'll compare the remote to it.
	localHash, err := sha256File(localBin)
	if err != nil {
		fatalf("hash local agent: %v", err)
	}

	remotePath := paths.RemoteAgentPath
	tmpRemote := remotePath + ".new"

	// Idempotent fast path: if not --force and the remote hash already matches,
	// skip the upload. This makes repeated `ensure` cheap and safe.
	if !force {
		if remoteHash, ok := remoteSHA256(host, remotePath); ok {
			if remoteHash == localHash {
				fmt.Fprintf(os.Stderr, "rsb: %s agent already up to date (%.12s)\n", host, localHash)
				return
			}
			fmt.Fprintf(os.Stderr, "rsb: %s agent is stale (%.12s -> %s), updating\n",
				host, remoteHash, localHash[:12])
		}
	}

	// Upload to a temp path, chmod, then atomic mv into place. A failed scp
	// leaves only the .new file, never touching the live agent.
	fmt.Fprintf(os.Stderr, "rsb: installing %s -> %s:%s\n", localBin, host, remotePath)
	steps := [][]string{
		{"ssh", host, "mkdir -p ~/.rsb"},
		{"scp", localBin, host + ":" + tmpRemote},
		{"ssh", host, "chmod +x " + tmpRemote},
		// mv is atomic on POSIX: at no instant does a half-installed agent exist.
		{"ssh", host, "mv -f " + tmpRemote + " " + remotePath},
	}
	for _, c := range steps {
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			// Clean up the temp file so the next run isn't confused by it.
			exec.Command("ssh", host, "rm -f "+tmpRemote).Run()
			ensureFail(host, "%s: %v", c[0], err)
		}
	}

	// Verify: hash the freshly-installed remote agent and compare. This is the
	// critical guard — it catches truncation, partial scp, wrong-platform
	// binaries, and any silent failure that "looked" successful.
	remoteHash, ok := remoteSHA256(host, remotePath)
	if !ok {
		ensureFail(host, "installed but could not hash remote agent — verification FAILED\n"+
			"the agent may be corrupt; retry: rsb ensure %s --force", host)
	}
	if remoteHash != localHash {
		ensureFail(host, "verification FAILED: remote agent sha256 mismatch\n"+
			"  local:  %s\n  remote: %s\n"+
			"the remote agent is NOT what we uploaded. retry: rsb ensure %s --force",
			localHash, remoteHash, host)
	}
	fmt.Fprintf(os.Stderr, "rsb: %s ready (verified sha256 %.12s)\n", host, localHash)
}

// ensureFail reports an ensure failure WITH a downgrade hint, so the user
// isn't stuck. Even without a working agent, rsb exec still works for raw
// commands (including manual `docker exec`), and rsb cp/sync fall back to
// scp. Borrowed from Warp's "Failed → fallback" state-machine philosophy:
// don't hard-stop, give a path forward.
func ensureFail(host, format string, args ...any) {
	fmt.Fprintf(os.Stderr, "rsb: "+format+"\n\n", args...)
	fmt.Fprintf(os.Stderr, "rsb: downgrade options while agent is unavailable:\n")
	fmt.Fprintf(os.Stderr, "  - exec raw commands:    rsb exec %s -- <cmd>\n", host)
	fmt.Fprintf(os.Stderr, "  - manual container:     rsb exec %s -- docker exec <container> <cmd>\n", host)
	fmt.Fprintf(os.Stderr, "  - file transfer:        use scp until agent is fixed\n")
	fmt.Fprintf(os.Stderr, "  - retry install:        rsb ensure %s --force\n", host)
	os.Exit(1)
}

// sha256File returns the hex sha256 of a local file.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// remoteSHA256 runs `sha256sum` (or `shasum -a 256` fallback) on the host and
// returns the hex hash. ok=false if the file is missing or the command failed.
func remoteSHA256(host, path string) (hash string, ok bool) {
	// Try sha256sum first (Linux), fall back to shasum (macOS/BSD).
	for _, cmd := range []string{"sha256sum " + path, "shasum -a 256 " + path} {
		out, err := exec.Command("ssh", host, cmd).Output()
		if err != nil {
			continue
		}
		// Both commands print "<hash>  <path>" or "<hash>  <path>"; first field.
		fields := strings.Fields(string(out))
		if len(fields) > 0 && len(fields[0]) == 64 {
			return fields[0], true
		}
	}
	return "", false
}

// cmdAgentVersion: rsb agent-version <host> — runs `rsb-agent --version` on the
// remote host over ssh and prints its version/protocol/build-time. This is the
// reliable way to answer "did ensure actually update the agent?" without
// guessing from behavior (pain point #10).
func cmdAgentVersion(args []string) {
	host := "local"
	rest := args
	if len(rest) > 0 && rest[0] == "--local" {
		rest = rest[1:]
	} else if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		host = rest[0]
		rest = rest[1:]
	}

	// Resolve the agent binary path on the target. Remote: the install path.
	// Local: via home discovery so it works from any cwd.
	agentBin := paths.RemoteAgentPath
	if host == "local" {
		if dir, err := paths.LocalPlatformDir(); err == nil {
			agentBin = dir + "/rsb-agent"
		} else {
			fatalf("cannot find local agent: %v", err)
		}
	}

	var out []byte
	var err error
	if host == "local" {
		out, err = exec.Command(agentBin, "--version").Output()
	} else {
		out, err = exec.Command("ssh", host, agentBin+" --version").Output()
	}
	if err != nil {
		// Distinguish "agent not installed" from other failures.
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		fmt.Fprintf(os.Stderr, "rsb: cannot get agent version on %s: %v\n", host, err)
		if strings.Contains(stderr, "No such file") || strings.Contains(stderr, "not found") {
			fmt.Fprintf(os.Stderr, "rsb: agent is not installed; run: rsb ensure %s\n", host)
		}
		os.Exit(1)
	}
	ver := strings.TrimSpace(string(out))
	if host == "local" {
		fmt.Printf("local:   %s\n", ver)
		fmt.Printf("client:  rsb %s (protocol v%d)\n", version, protocol.ProtocolVersion)
	} else {
		fmt.Printf("%s: %s\n", host, ver)
		fmt.Printf("client:  rsb %s (protocol v%d)\n", version, protocol.ProtocolVersion)
	}
}

// cmdDoctor runs a self-check: home discovery, this-platform binaries, the
// cmdDoctor runs a self-check: home discovery, this-platform binaries, the
// daemon, optional SSH host reachability + remote agent version/hash, and
// docker access with an optional real container smoke test. Exits non-zero
// if any check fails.
//
// Usage: rsb doctor [host] [--container NAME]
//   host       : if given, also checks SSH reachability, remote agent
//                version+hash (vs local), and remote container mode.
//   --container: if given, runs a REAL `docker exec <NAME> true` smoke test
//                instead of just checking daemon connectivity. This catches
//                the "doctor says ok but --container actually fails" trap.
func cmdDoctor(args []string) {
	host := ""
	testContainer := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--local":
			host = "local"
		case strings.HasPrefix(a, "--container="):
			testContainer = strings.TrimPrefix(a, "--container=")
		case a == "--container":
			if i+1 >= len(args) {
				fatalf("usage: rsb doctor [host] [--container NAME]")
			}
			i++
			testContainer = args[i]
		case !strings.HasPrefix(a, "-"):
			host = a
		}
	}
	ok := true
	report := func(label, status, detail string) {
		mark := "ok"
		if status != "ok" {
			mark = status
			ok = false
		}
		if detail != "" {
			fmt.Fprintf(os.Stderr, "  %-30s %-10s %s\n", label+":", mark, detail)
		} else {
			fmt.Fprintf(os.Stderr, "  %-30s %s\n", label+":", mark)
		}
	}
	info := func(label, detail string) {
		fmt.Fprintf(os.Stderr, "  %-30s %s\n", label+":", detail)
	}

	fmt.Fprintf(os.Stderr, "rsb %s doctor\n", version)

	// 1. Install home discovery.
	home := paths.Home()
	if home == "" {
		report("install home", "FAIL", "not found (set RSB_HOME or run from install dir)")
	} else {
		report("install home", "ok", home)
	}

	// 2. This-platform binaries.
	if dir, err := paths.LocalPlatformDir(); err == nil {
		for _, bin := range []string{"rsb", "rsb-daemon", "rsb-agent"} {
			p := dir + "/" + bin
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				report(bin, "ok", p)
			} else {
				report(bin, "missing", "expected at "+p)
			}
		}
	} else {
		report("platform binaries", "FAIL", err.Error())
	}

	// 3. Daemon status (informational, not a failure if down).
	pid := readPID()
	if pid > 0 && processAlive(pid) {
		report("daemon", "ok", fmt.Sprintf("pid=%d socket=%s", pid, paths.SocketPath()))
	} else {
		info("daemon", "not running (auto-starts on first exec)")
	}

	// 4. Remote host checks: reachability, agent version + hash, container mode.
	if host != "" && host != "local" {
		out, err := exec.Command("ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5",
			host, "echo rsb-ssh-ok").Output()
		if err != nil {
			report("ssh "+host, "FAIL",
				"cannot connect non-interactively (key/agent needed)")
		} else if strings.TrimSpace(string(out)) == "rsb-ssh-ok" {
			report("ssh "+host, "ok", "reachable")

			// Remote agent present?
			if _, err := exec.Command("ssh", host, "test -x "+paths.RemoteAgentPath).CombinedOutput(); err != nil {
				report("remote agent", "missing", "run: rsb ensure "+host)
			} else {
				// Remote agent version (P10): the key check — is it the version
				// we think it is?
				rVer, _ := exec.Command("ssh", host, paths.RemoteAgentPath+" --version").Output()
				report("remote agent version", "ok", strings.TrimSpace(string(rVer)))

				// Remote agent hash vs local (P8/P10): catches stale installs.
				remoteOS, remoteArch, _ := probeRemoteArch(host)
				localBin := paths.AgentForPlatform(remoteOS, remoteArch)
				if localBin != "" {
					localHash, _ := sha256File(localBin)
					remoteHash, rhok := remoteSHA256(host, paths.RemoteAgentPath)
					if rhok && remoteHash == localHash {
						report("remote agent hash", "ok",
							fmt.Sprintf("%.12s (matches local)", localHash))
					} else if rhok {
						report("remote agent hash", "STALE",
							fmt.Sprintf("remote %.12s != local %.12s; fix: rsb ensure %s --force",
								remoteHash, localHash, host))
					}
				}
			}
		}
	}

	// 5. Docker access + container mode. P9: if --container given, run a REAL
	// smoke test (`docker exec <container> true`) so "doctor says ok" actually
	// means "--container will work", not just "docker daemon is up".
	localMode := "docker exec (default)"
	if strings.ToLower(os.Getenv("RSB_CONTAINER_MODE")) == "nsenter" {
		localMode = "nsenter (RSB_CONTAINER_MODE)"
	}
	if testContainer != "" && host != "" && host != "local" {
		if err := exec.Command("ssh", host, "docker", "exec", testContainer, "true").Run(); err != nil {
			report("container exec "+testContainer, "FAIL",
				"remote docker exec failed; "+strings.TrimSpace(err.Error())+
					"\n      fix: rsb ensure "+host+" --force, or use argv form: rsb exec "+host+" --argv '[\"docker\",\"exec\",\""+testContainer+"\",\"cmd\"]'")
		} else {
			report("container exec "+testContainer, "ok",
				"remote smoke test passed; local mode: "+localMode)
		}
	} else if _, err := exec.LookPath("docker"); err != nil {
		info("docker", "absent (container features unavailable)")
	} else if out, err := exec.Command("docker", "info").CombinedOutput(); err != nil {
		low := strings.ToLower(string(out))
		if strings.Contains(low, "permission denied") || strings.Contains(low, "docker.sock") {
			user := os.Getenv("USER")
			if user == "" {
				user = "<user>"
			}
			report("docker", "FAIL", "socket permission denied; fix: sudo usermod -aG docker "+user)
		} else {
			report("docker", "FAIL", "daemon not reachable")
		}
	} else if testContainer != "" {
		// REAL container smoke test: actually exec into the named container.
		// This is what distinguishes "docker daemon up" from "--container works".
		if err := exec.Command("docker", "exec", testContainer, "true").Run(); err != nil {
			report("container exec "+testContainer, "FAIL",
				"docker exec failed; "+strings.TrimSpace(err.Error())+
					"\n      fix: rsb ensure <host> --force, or use argv form: rsb exec <host> --argv '[\"docker\",\"exec\",\""+testContainer+"\",\"cmd\"]'")
		} else {
			report("container exec "+testContainer, "ok",
				"smoke test passed; local mode: "+localMode)
		}
	} else {
		// No container specified: just report the mode. Note this is the LOCAL
		// expected mode; the actual remote mode depends on the remote agent
		// version (an old agent ignores RSB_CONTAINER_MODE).
		report("docker", "ok", "local mode: "+localMode+" (use --container=NAME for a real test)")
	}

	if ok {
		fmt.Fprintf(os.Stderr, "\nrsb: all checks passed\n")
	} else {
		fmt.Fprintf(os.Stderr, "\nrsb: some checks FAILED (see above)\n")
		os.Exit(1)
	}
}

// cmdInstallLocal creates convenience symlinks bin/rsb, bin/rsb-daemon,
// bin/rsb-agent pointing at the current platform's binaries. After this, the
// caller can invoke <home>/bin/rsb directly without picking the platform dir,
// and PATH=$HOME/.codex/skills/rsb/bin:$PATH "just works".
func cmdInstallLocal(args []string) {
	dir, err := paths.LocalPlatformDir()
	if err != nil {
		fatalf("cannot find platform binaries: %v", err)
	}
	home := paths.Home()
	if home == "" {
		fatalf("cannot find rsb home (set RSB_HOME or run from install dir)")
	}
	binDir := home + "/bin"
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		fatalf("mkdir %s: %v", binDir, err)
	}
	for _, bin := range []string{"rsb", "rsb-daemon", "rsb-agent"} {
		target := dir + "/" + bin
		link := binDir + "/" + bin
		if _, err := os.Stat(target); err != nil {
			fatalf("source missing: %s", target)
		}
		os.Remove(link) // refresh existing symlink
		if err := os.Symlink(target, link); err != nil {
			fatalf("symlink %s -> %s: %v", link, target, err)
		}
		fmt.Fprintf(os.Stderr, "  %s -> %s\n", link, target)
	}
	fmt.Fprintf(os.Stderr, "rsb: installed. add to PATH: export PATH=%s:$PATH\n", binDir)
}

// probeRemoteArch asks the host for its OS and machine arch via ssh.
func probeRemoteArch(host string) (osName, arch string, err error) {
	out, err := exec.Command("ssh", host, "uname -sm").Output()
	if err != nil {
		return "", "", err
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) < 2 {
		return "", "", fmt.Errorf("unexpected uname output: %q", string(out))
	}
	osName = strings.ToLower(fields[0])
	arch = normalizeArch(fields[1])
	return osName, arch, nil
}

// normalizeArch maps uname -m outputs to the GOARCH names used in our dist dirs.
func normalizeArch(m string) string {
	switch m {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	default:
		return strings.ToLower(m)
	}
}

// --- helpers ---

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "rsb: "+format+"\n", args...)
	os.Exit(1)
}

func newID() string {
	// Cheap unique-enough id: PID + nanoseconds. No uuid dep needed.
	return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
}

// --- daemon process management (for `rsb daemon stop|status`) ---

func readPID() int {
	b, err := os.ReadFile(paths.PIDFile())
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

func processAlive(pid int) bool {
	// kill 0 checks process existence without sending a signal.
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	return true
}

func killPID(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}

// multiFlag is a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(s string) error { *m = append(*m, s); return nil }
