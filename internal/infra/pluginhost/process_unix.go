//go:build !windows

package pluginhost

import (
	"os"
	"os/exec"
	"syscall"
)

// configureProcess is a no-op outside Windows: the host has no console window to
// hide.
func configureProcess(_ *exec.Cmd) {}

// terminateProcess asks the plugin to exit with SIGTERM. The caller has already
// closed stdin and waits out the shutdown grace period before escalating to
// Kill.
func terminateProcess(process *os.Process) error {
	return process.Signal(syscall.SIGTERM)
}
