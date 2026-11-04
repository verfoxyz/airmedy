package pluginhost

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"airmedy/internal/domain"
)

// process is one running plugin child process and the pipes the transport uses.
//
// The wait goroutine is the single owner of cmd.Wait: it records the exit error
// and closes done, and every other goroutine learns about the exit from that
// channel rather than calling Wait again.
type process struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser
	logger *slog.Logger
	id     string

	done    chan struct{}
	waitErr error

	workers sync.WaitGroup
}

// startProcess launches the plugin described by manifest.
//
// The process inherits the host environment, then manifest env, then extraEnv,
// so a test helper can be selected by an environment variable without changing
// the plugin's own configuration.
func startProcess(manifest domain.PluginManifest, logger *slog.Logger, extraEnv []string) (*process, error) {
	cmd := exec.Command(manifest.Runtime.Command, manifest.Runtime.Args...)
	cmd.Dir = manifest.Dir
	cmd.Env = buildEnv(manifest.Runtime.Env, extraEnv)
	configureProcess(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("start plugin %q: stdin pipe: %w", manifest.ID, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("start plugin %q: stdout pipe: %w", manifest.ID, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("start plugin %q: stderr pipe: %w", manifest.ID, err)
	}

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("start plugin %q (%s): %w", manifest.ID, manifest.Runtime.Command, err)
	}

	p := &process{
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
		stderr: stderr,
		logger: logger,
		id:     manifest.ID,
		done:   make(chan struct{}),
	}

	p.workers.Add(2)
	go p.wait()
	go p.pumpStderr()
	return p, nil
}

// buildEnv layers the plugin's configured environment over the host's.
func buildEnv(pluginEnv map[string]string, extra []string) []string {
	env := os.Environ()
	for key, value := range pluginEnv {
		env = append(env, key+"="+value)
	}
	return append(env, extra...)
}

func (p *process) wait() {
	defer p.workers.Done()
	p.waitErr = p.cmd.Wait()
	close(p.done)
}

// pumpStderr forwards plugin log lines to the host logger at debug level with
// the plugin id attached, as the v1 transport contract specifies.
func (p *process) pumpStderr() {
	defer p.workers.Done()
	scanner := bufio.NewScanner(p.stderr)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		p.logger.Debug("Plugin stderr", "plugin", p.id, "line", scanner.Text())
	}
	if err := scanner.Err(); err != nil && p.logger != nil {
		p.logger.Debug("Plugin stderr stream ended", "plugin", p.id, "error", err)
	}
}

// exited reports whether the process has already exited.
func (p *process) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// waitExit blocks until the process exits or timeout elapses.
func (p *process) waitExit(timeout time.Duration) bool {
	if timeout <= 0 {
		<-p.done
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.done:
		return true
	case <-timer.C:
		return false
	}
}

// exitError returns the error cmd.Wait reported. Only meaningful after done is
// closed.
func (p *process) exitError() error {
	<-p.done
	return p.waitErr
}

// closeStdin sends EOF to the plugin, which well-behaved plugins treat as a
// request to exit.
func (p *process) closeStdin() {
	_ = p.stdin.Close()
}

// terminate asks the process to stop, falling back to a hard kill where the
// platform has no polite signal.
func (p *process) terminate() {
	p.closeStdin()
	if err := terminateProcess(p.cmd.Process); err != nil {
		p.logger.Debug("Plugin process termination signal failed, killing",
			"plugin", p.id, "error", err)
		_ = p.cmd.Process.Kill()
	}
}

// kill stops the process immediately.
func (p *process) kill() {
	p.closeStdin()
	_ = p.cmd.Process.Kill()
}

// waitWorkers waits for the wait and stderr goroutines to finish. It is called
// by Close once the process is gone, so a closed host owns no goroutines.
func (p *process) waitWorkers() {
	p.workers.Wait()
}
