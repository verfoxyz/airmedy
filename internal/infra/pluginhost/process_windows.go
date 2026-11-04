//go:build windows

package pluginhost

import (
	"os"
	"os/exec"
	"syscall"
)

// configureProcess keeps a plugin launch from flashing a console window in the
// GUI build and detaches the child from the host's console.
func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
}

// terminateProcess has no polite signal on Windows. Closing stdin is the
// graceful half of the shutdown and is done by the caller; the hard stop is the
// only fallback, so the grace period is spent waiting for the process to exit on
// its own.
func terminateProcess(process *os.Process) error {
	return process.Kill()
}
