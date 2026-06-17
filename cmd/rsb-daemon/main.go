// Command rsb-daemon is the local persistent process that maintains SSH
// connections to remote hosts and multiplexes client requests.
//
// It is normally started on demand by `rsb exec` (the first invocation spawns
// it if the socket isn't serving). Run manually with `rsb daemon`.
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"rsb/internal/daemon"
	"rsb/internal/paths"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetPrefix("rsb-daemon: ")
	flag.Parse()

	d, err := daemon.New()
	if err != nil {
		log.Fatalf("start: %v", err)
	}
	// Record PID for `rsb daemon stop`.
	os.WriteFile(paths.PIDFile(), []byte(fmtPid(os.Getpid())), 0o600)

	// Stop on SIGTERM/SIGINT, and also if the socket is removed externally.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		log.Printf("shutting down")
		d.Close()
		os.Remove(paths.PIDFile())
	}()

	log.Printf("started pid=%d", os.Getpid())
	d.Serve()
}

func fmtPid(pid int) []byte {
	return []byte(itoa(pid) + "\n")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
