// Package paths centralizes filesystem locations used by rsb: the install
// "home" directory, the daemon socket/pid, and the remote agent path.
//
// The most important concept here is Home() — discovering where rsb is
// installed so binaries can find each other (rsb finds rsb-daemon; `ensure`
// finds the platform rsb-agent) regardless of the caller's current directory.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// RuntimeDir is the local per-user directory for rsb state (socket, pid).
func RuntimeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	return filepath.Join(home, ".rsb")
}

// SocketPath is the unix-domain socket the daemon listens on.
func SocketPath() string {
	return filepath.Join(RuntimeDir(), "daemon.sock")
}

// PIDFile records the running daemon's PID, for stop/healthcheck.
func PIDFile() string {
	return filepath.Join(RuntimeDir(), "daemon.pid")
}

// RemoteAgentPath is where `rsb ensure` installs rsb-agent on a remote host.
const RemoteAgentPath = ".rsb/rsb-agent"

// EnsureRuntimeDir creates the runtime dir if missing.
func EnsureRuntimeDir() error {
	return os.MkdirAll(RuntimeDir(), 0o700)
}

// ---- install home discovery ----

// homeEnvVar is the environment variable a user or skill can set to pin the
// install location explicitly. Takes priority over auto-detection.
const homeEnvVar = "RSB_HOME"

var (
	homeOnce  sync.Once
	homeCache string
)

// Home returns rsb's install root — the directory containing `bin/`,
// `SKILL.md`, etc. Discovery uses a priority chain so the binaries work no
// matter where the caller's cwd is:
//
//  1. RSB_HOME env var (explicit override, e.g. set by a skill wrapper)
//  2. executable path inference (the robust default):
//     - if this binary sits at <home>/bin/<os>-<arch>/rsb* -> home = <home>
//     - if it sits at <home>/bin/rsb* (post install-local symlink) -> home
//  3. cwd fallback (legacy/dev: ./bin, ./skill/bin must exist)
//
// Returns "" only if nothing resolves, in which case callers should surface a
// clear "cannot find rsb home" error rather than guessing.
func Home() string {
	homeOnce.Do(func() {
		homeCache = resolveHome()
	})
	return homeCache
}

// resolveHome implements the priority chain. Split out so tests can reset.
func resolveHome() string {
	// 1. Explicit env override.
	if h := os.Getenv(homeEnvVar); h != "" {
		if isHome(h) {
			return h
		}
	}

	// 2. Infer from this binary's real location on disk.
	if h := inferHomeFromExecutable(); h != "" {
		return h
	}

	// 3. Cwd fallback for dev/legacy layouts.
	for _, candidate := range []string{".", ".."} {
		if isHome(candidate) {
			if abs, err := filepath.Abs(candidate); err == nil {
				return abs
			}
		}
	}
	return ""
}

// inferHomeFromExecutable resolves the running binary's path and walks up to
// find the install root. Supports both layouts:
//
//	<home>/bin/<os>-<arch>/rsb        (skill/dist layout)
//	<home>/bin/rsb                    (post install-local symlink layout)
func inferHomeFromExecutable() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	// Resolve symlinks: if `rsb` is a symlink to darwin-arm64/rsb, we want the
	// real file's location so the ../.. math works.
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		real = exe
	}
	dir := filepath.Dir(real) // .../bin/<os>-<arch>  or  .../bin

	// Layout 1: <home>/bin/<os>-<arch>/rsb -> up two levels.
	parent := filepath.Dir(dir) // .../bin
	grand := filepath.Dir(parent) // .../<home>
	if isHome(grand) {
		return grand
	}
	// Layout 2: <home>/bin/rsb -> up one level.
	if isHome(parent) {
		return parent
	}
	return ""
}

// isHome reports whether dir looks like an rsb install root: it must contain
// a bin/ subdirectory. SKILL.md/scripts may or may not be present (a minimal
// dist-only layout is valid).
func isHome(dir string) bool {
	if dir == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(dir, "bin"))
	return err == nil && info.IsDir()
}

// LocalPlatformDir returns the bin subdirectory matching the current runtime
// (e.g. "<home>/bin/linux-amd64"). This is where sibling binaries for THIS
// platform live (rsb finds rsb-daemon here).
func LocalPlatformDir() (string, error) {
	h := Home()
	if h == "" {
		return "", fmt.Errorf("cannot locate rsb install home (set %s or run from the install dir)", homeEnvVar)
	}
	platforms := []string{
		filepath.Join(h, "bin", currentOS()+"-"+currentArch()),
		// post install-local: binaries sit directly in bin/
		filepath.Join(h, "bin"),
	}
	for _, p := range platforms {
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("no bin directory under %s", h)
}

// AgentForPlatform returns the path to the rsb-agent binary for a given
// target os/arch, resolved under home/bin/<os>-<arch>/. Returns "" if absent.
func AgentForPlatform(osName, arch string) string {
	h := Home()
	c := filepath.Join(h, "bin", osName+"-"+arch, "rsb-agent")
	if st, err := os.Stat(c); err == nil && !st.IsDir() {
		return c
	}
	return ""
}

func currentOS() string  { return runtime.GOOS }
func currentArch() string { return runtime.GOARCH }
