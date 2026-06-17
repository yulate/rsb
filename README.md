# rsb — remote shell bridge for AI agents

<p align="center">
  <strong>Run commands on remote hosts & containers without quoting hell.</strong><br>
  argv travels as a JSON array to <code>execve</code> on the target — never through a shell.
</p>

<p align="center">
  <strong>English</strong> · <a href="docs/README.zh-CN.md">中文</a>
</p>

---

`rsb` lets LLM agents (Claude Code, Codex, Cursor, …) operate remote servers
the same way they operate the local machine. It kills the two problems that
make `ssh host "cmd"` unusable for agents:

1. **Quoting hell.** A command string passes through *local shell → remote
   shell → (optional) container shell*, and escaping compounds at each layer.
   No LLM can reliably produce `ssh prod "docker exec api sh -c 'echo \"$X\"'"`
   across three shells.
2. **State loss.** Every `ssh host "cmd"` is a fresh login shell. `cd /app`
   followed by `ssh host "ls"` lands back in `~`. Agents can't express "keep
   working in this directory."

**rsb's fix:** commands are a `[]string` argv, sent as a length-delimited
JSON frame to a small agent process on the remote host, which `execve`s it
directly. No shell ever parses it — so there's no escaping layer to get wrong.
A persistent daemon + sessions make `cd` and env stick across commands.

> Inspired by [Warp](https://www.warp.dev)'s SSH extension (companion process
> on the remote, structured protocol, one server many sessions) — but rsb
> targets **agent-native remote execution**, not terminal UI.

## The 10-second pitch

```bash
# This command contains double quotes, single quotes, and a $ variable.
# Over ssh it would be a quoting nightmare. Over rsb it just works —
# argv is an array, so every character arrives at the target verbatim.
rsb exec prod --argv '["echo","he said \"hi\" and '"'"'yo'"'"', $HOME stays literal"]'
# => he said "hi" and 'yo', $HOME stays literal
```

Same command for a container — `--container` keeps argv intact into the
container's process, still no shell:

```bash
rsb exec prod --container api --argv '["env"]'
```

## Quickstart

```bash
# 1. Build (or grab a prebuilt binary from releases)
go build -o bin/rsb ./cmd/rsb
go build -o bin/rsb-agent ./cmd/rsb-agent
go build -o bin/rsb-daemon ./cmd/rsb-daemon

# 2. One-time: symlink the current platform's binaries into bin/
rsb install-local

# 3. Verify your setup
rsb doctor                        # self-check: home, binaries, daemon, docker
rsb doctor prod --container=api   # also probe a remote host + real container test

# 4. Install the agent on a remote host (probes OS/arch, scp's matching binary, sha256-verifies)
rsb ensure prod

# 5. Run commands
rsb exec prod --argv '["kubectl","logs","deploy/api","--tail=50"]'
rsb exec prod --session work --argv '["cd","/opt/app"]'
rsb exec prod --session work --argv '["ls"]'    # still in /opt/app
```

## How it works

```
your agent / shell
   │  rsb exec  (or rsb repl)
   ▼
local daemon ──unix socket──►  rsb-daemon ──ssh──►  rsb-agent (remote, persistent)
                               (1 conn per host,      ├─ execve(argv)   ← no shell
                                connection reuse)     ├─ session table  ← cwd/env persist
                                                      └─ docker adapter ← argv into container
```

- **Zero new ports.** The daemon talks over a unix socket; the remote agent is
  a child process of `sshd`, communicating over the existing SSH channel's
  stdin/stdout. If you can `ssh` to a host, rsb works — no firewall changes.
- **argv to execve.** The protocol carries `[]string`, not a command string.
  The agent resolves `argv[0]` via `PATH` and calls `execve`. Quotes, `$`,
  spaces, backticks — none of them are "special", because nothing parses them.
- **Persistent daemon + sessions.** One SSH connection per host is kept open
  and reused. Sessions serialize commands in the same session (so `cd` is
  meaningful) while different sessions run concurrently.

## Features

| | Status |
|---|---|
| argv array → execve (no shell, zero quoting) | ✅ |
| Persistent daemon + connection reuse | ✅ |
| Sessions: `cd` / cwd / env persist across commands | ✅ |
| Streaming stdin (pipes, interactive) | ✅ |
| Docker container execution (`--container`) | ✅ |
| Multi-request concurrency + cancel | ✅ |
| Interactive REPL (`rsb repl`) | ✅ |
| `ensure` with atomic install + sha256 verification | ✅ |
| `doctor` self-check with real container smoke test | ✅ |
| Remote agent version + hash visibility | ✅ |
| Cross-platform prebuilt binaries (linux/macOS × amd64/arm64) | ✅ |
| Kubernetes (`kubectl exec`) adapter | 🔜 |
| Compose service-name resolution | 🔜 |

## Why not …?

| Tool | What it solves | What it leaves |
|---|---|---|
| `ssh host "cmd"` / ansible / fabric | wrapping ssh | still a command *string* — quoting hell intact |
| paramiko / asyncssh | SSH library | `exec_command(string)` — shell parsing problem remains |
| tmux Control Mode (Warp legacy) | connection reuse | terminal-UI oriented, not an agent exec API |
| Warp SSH extension | structured protocol + reuse | closed-source, terminal-user oriented |
| VS Code Remote-SSH | structured protocol | IDE-specific, not an agent API |
| **rsb** | **argv to execve + daemon + sessions + containers** | — |

rsb sits in an empty niche: an **agent-native remote execution runtime** that's
open, single-binary, and treats argv as a first-class citizen end to end.

## Installation

### From source

```bash
git clone https://github.com/<owner>/rsb.git
cd rsb
./scripts/build.sh          # builds 3 binaries for 3 platforms into skill/bin/
```

### Prebuilt binaries

Grab the archive for your platform from [Releases](../../releases). Each
archive contains `rsb`, `rsb-agent`, `rsb-daemon` for one platform. Drop them
in a directory and run `rsb install-local`.

### As an agent skill

The `skill/` directory is a self-contained, agent-installable package:
`SKILL.md` teaches the agent when and how to use rsb; `bin/` holds prebuilt
binaries for all platforms. Copy `skill/` into your agent's skills directory
(e.g. `~/.codex/skills/rsb/`).

## Usage reference

```
rsb exec <host> --argv '<json>' [options]
  --argv '<json>'     (required) JSON string array, e.g. '["ls","-la"]'
  --cwd DIR           working directory on the target
  --env K=V           environment variable (repeatable); values are NOT expanded
  --timeout MS        kill the command after N milliseconds
  --session NAME      share cwd/env across commands; "cd" persists per session
  --container NAME    run inside a Docker container (argv reaches it verbatim)
  --stdin             pipe local stdin to the remote command
  --local             run on this machine (no SSH)

rsb repl <host> [--session NAME]              interactive multi-command session
rsb ensure <host> [--force]                   install/upgrade the remote agent (sha256-verified)
rsb agent-version <host>                      show the agent version on a host
rsb doctor [host] [--container=NAME]          self-check (real container smoke test if --container)
rsb install-local                             symlink current-platform binaries into bin/
rsb daemon status|stop                        manage the local daemon (usually automatic)
rsb version

run `rsb <command> --help` for detailed per-command help.
```

**Exit code:** `rsb exec`'s exit code equals the remote command's exit code.

### Container mode

By default rsb enters containers via `docker exec` (works for unprivileged
users with docker-group access). For root/privileged hosts, set
`RSB_CONTAINER_MODE=nsenter` to use `nsenter` directly (faster, skips the
docker daemon round-trip).

If `--container` fails with `nsenter: Permission denied`, your remote agent is
likely a **stale (pre-0.5.0) version** that defaulted to nsenter. Fix it:

```bash
rsb ensure <host> --force          # upgrade + verify
rsb agent-version <host>           # confirm it matches
```

Fallback that always works (still argv, still no shell):

```bash
rsb exec prod --argv '["docker","exec","api","ls","/app"]'
```

## How argv survives (the core invariant)

```
agent code (Python/JS/shell)
   │  argv = ["echo", 'has "double" and $HOME']   ← you write a list
   │  json.dumps(argv)                            ← JSON escaping is deterministic
   ▼
JSON wire: ["echo","has \"double\" and $HOME"]
   │  rsb carries it as a length-delimited frame
   ▼
remote execve(["echo", 'has "double" and $HOME'])  ← restored exactly, no shell
   ▼
echo receives: has "double" and $HOME             ← correct
```

Contrast with traditional SSH, where the same intent must survive N shell
parsers, each with its own escaping rules, and the agent must mentally simulate
the result of every layer. rsb removes the parsers entirely.

## Project layout

```
cmd/
  rsb/          client CLI (exec, repl, ensure, doctor, …)
  rsb-agent/    remote daemon (multi-request, sessions, execve, docker)
  rsb-daemon/   local daemon (connection pool, frame routing)
internal/
  protocol/     length-delimited JSON framing + message types
  daemon/       host connection pool + client bridge
  client/       daemon connect / autostart / streaming stdin / repl
  docker/       container adapter (docker exec default, nsenter opt-in)
  paths/        install-home discovery (RSB_HOME > executable > cwd)
skill/          self-contained agent-installable package
docs/           changelog + pain-point records
scripts/        cross-compile script (3 platforms)
```

## Roadmap

- [ ] Kubernetes adapter (`--container` → `kubectl exec`)
- [ ] Compose service-name resolution (`--container api` → `project-api-1`)
- [ ] `rsb scp` (file transfer over the same connection)
- [ ] Per-host allowlist / audit hook for destructive commands

## License

MIT
