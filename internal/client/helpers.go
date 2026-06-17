package client

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"rsb/internal/paths"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// isNoSuchFile reports whether an error is "file does not exist".
func isNoSuchFile(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such file")
}

// findDaemonBinary locates the rsb-daemon binary via the install home
// discovery chain (RSB_HOME > executable path > cwd), then PATH. Works no
// matter where the caller's cwd is — the key fix for pain point #2.
func findDaemonBinary() string {
	// 1. Via install home: look in the same platform dir as this binary,
	//    then in bin/ (post install-local symlink layout).
	if dir, err := paths.LocalPlatformDir(); err == nil {
		p := filepath.Join(dir, "rsb-daemon")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	// 2. PATH lookup (e.g. installed system-wide).
	if p, err := exec.LookPath("rsb-daemon"); err == nil {
		return p
	}
	return ""
}

// detachAttr returns the SysProcAttr that detaches the daemon into its own
// process group/session so it survives the parent exiting.
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
