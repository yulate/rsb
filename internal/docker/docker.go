// Package docker is rsb-agent's container execution adapter.
//
// When a Request has a non-empty Container field, the agent routes execution
// here instead of execve-ing on the host. The goal — and the reason this
// adapter exists at all — is to keep the argv-array-reaches-execve property
// that kills quoting hell, even when the target is inside a container.
//
// Strategy selection (P3, revised after real-world feedback):
//
//  1. docker exec (DEFAULT): `docker exec -i <container> <argv...>`. Docker's
//     exec API takes an argv array (not a shell string), so no shell parses
//     our argv. This works for unprivileged users with docker group access —
//     the common case on real servers. nsenter needs root/CAP_SYS_ADMIN and
//     fails with "Permission denied" for normal users, so it is NOT the
//     default.
//
//  2. nsenter (opt-in via RSB_CONTAINER_MODE=nsenter): resolve the
//     container's main PID and run argv under nsenter. Faster (no docker
//     daemon round-trip) and reaches execve directly, but requires
//     privileges. Power users who run rsb-agent as root can select it.
//
// Both paths receive the caller's argv as an array and never join it into a
// shell string. That's the whole point of rsb, preserved through the
// container boundary.
package docker

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Mode selects how to enter the container.
type Mode int

const (
	// ModeDocker uses `docker exec`. Default; works for unprivileged users.
	ModeDocker Mode = iota
	// ModeNsenter uses nsenter into the container's namespaces. Faster but
	// requires root/CAP_SYS_ADMIN.
	ModeNsenter
)

// ErrNoContainerRuntime is returned when docker isn't available on the host.
var ErrNoContainerRuntime = errors.New("no container runtime available (docker not found)")

// SocketPermissionError is returned when docker is present but the current
// user can't access the docker socket. Distinct type so callers can detect it
// and print the usermod hint (pain point #7).
type SocketPermissionError struct{ Detail string }

func (e *SocketPermissionError) Error() string { return e.Detail }

// ResolveContainer maps a user-facing container name to the target the
// adapter can act on. Identity for exact names; resolveContainerName (called
// from BuildArgv) handles compose-service-name lookup.
func ResolveContainer(name string) (string, error) {
	if name == "" {
		return "", errors.New("empty container name")
	}
	return name, nil
}

// resolveContainerName turns a possibly-compose-service name into a real
// container name/id. If `name` is already a valid container, returned as-is.
// Otherwise looks it up by the com.docker.compose.service label, so
// `--container api` finds the `myproject-api-1` compose container.
func resolveContainerName(name string) string {
	// Is it a real running container? `docker inspect` is the cheapest check.
	if out, err := exec.Command("docker", "inspect", "-f", "{{.Id}}", name).Output(); err == nil {
		if id := strings.TrimSpace(string(out)); id != "" {
			return name // exact match
		}
	}
	// Compose service lookup: containers carry label
	// com.docker.compose.service=<service>. Match the first running one.
	out, err := exec.Command("docker", "ps", "--filter",
		"label=com.docker.compose.service="+name,
		"--format", "{{.Names}}").Output()
	if err != nil {
		return name // docker failed; let BuildArgv surface the real error
	}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l // first matching compose container
		}
	}
	return name // nothing resolved; use as given
}

// hasDocker reports whether docker is on PATH.
func hasDocker() bool {
	_, err := exec.LookPath("docker")
	return err == nil
}

// currentMode picks the container mode from the environment, defaulting to
// docker exec (the unprivileged-friendly path).
func currentMode() Mode {
	switch strings.ToLower(os.Getenv("RSB_CONTAINER_MODE")) {
	case "nsenter":
		return ModeNsenter
	default:
		return ModeDocker
	}
}

// containerPID returns the main PID of a container via `docker inspect`.
func containerPID(container string) (string, error) {
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Pid}}", container).Output()
	if err != nil {
		return "", err
	}
	pid := strings.TrimSpace(string(out))
	if pid == "0" || pid == "" {
		return "", fmt.Errorf("container %s not running", container)
	}
	return pid, nil
}

// checkDockerAccess runs a cheap docker command to see if the current user
// can actually talk to the daemon. Returns a SocketPermissionError when the
// failure looks like a socket-permission problem (pain point #7).
func checkDockerAccess() error {
	out, err := exec.Command("docker", "info").CombinedOutput()
	if err == nil {
		return nil
	}
	lower := strings.ToLower(string(out))
	if strings.Contains(lower, "permission denied") ||
		strings.Contains(lower, "connect to the docker daemon socket") ||
		strings.Contains(lower, "docker.sock") {
		user := os.Getenv("USER")
		if user == "" {
			user = "<user>"
		}
		return &SocketPermissionError{
			Detail: "cannot access Docker socket (permission denied)\n" +
				"fix: sudo usermod -aG docker " + user + ", then reconnect rsb daemon",
		}
	}
	return fmt.Errorf("docker daemon not reachable: %s", strings.TrimSpace(string(out)))
}

// BuildArgv decides how to run the caller's argv inside the container and
// returns the effective argv to execve on the host. It does NOT execute
// anything — the caller (rsb-agent) runs the returned argv with execve,
// which keeps the no-shell property end to end.
//
// Mode is chosen by currentMode() (RSB_CONTAINER_MODE env, default docker).
// Before building argv it validates docker access, surfacing a specific
// SocketPermissionError when the user lacks socket perms.
func BuildArgv(container string, userArgv []string) ([]string, error) {
	if len(userArgv) == 0 {
		return nil, errors.New("empty argv")
	}
	container, err := ResolveContainer(container)
	if err != nil {
		return nil, err
	}
	// Compose service-name resolution: turn "api" into "myproject-api-1" if
	// it's a compose service rather than a literal container name.
	container = resolveContainerName(container)
	if !hasDocker() {
		return nil, ErrNoContainerRuntime
	}

	mode := currentMode()

	// nsenter path: opt-in only. If it's requested but unavailable, fall back
	// to docker exec rather than failing hard — the user wants their command
	// to run, and docker exec is correct (just slightly slower).
	if mode == ModeNsenter {
		if _, err := exec.LookPath("nsenter"); err == nil {
			if pid, err := containerPID(container); err == nil {
				return append([]string{"nsenter", "-t", pid, "-a"}, userArgv...), nil
			}
		}
		// fall through to docker exec
	}

	// Default path: docker exec. Validate access first so we can give the
	// specific "usermod -aG docker" hint instead of a cryptic stderr dump.
	if err := checkDockerAccess(); err != nil {
		return nil, err
	}
	// -i keeps stdin wired for our streaming; argv array goes to docker's
	// exec API verbatim, no shell joining.
	return append([]string{"docker", "exec", "-i", container}, userArgv...), nil
}
