package appserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"
)

type ProcessOptions struct {
	Command string
	Version string
	Timeout time.Duration
}

// Open starts a local app-server process over its standard JSON-RPC transport.
// It uses the same Codex home as Desktop, so persisted desktop threads remain
// available through thread/list without exposing a network listener.
func Open(ctx context.Context, opts ProcessOptions) (*Client, error) {
	command := opts.Command
	if command == "" {
		command = "codex"
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 12 * time.Second
	}
	cmd := exec.CommandContext(ctx, command, "app-server", "--stdio")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open app-server stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open app-server stdout: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start app-server: %w", err)
	}

	var waitOnce sync.Once
	closeProcess := func() error {
		var waitErr error
		waitOnce.Do(func() {
			_ = stdin.Close()
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			waitErr = cmd.Wait()
			if waitErr != nil && !errors.Is(waitErr, exec.ErrWaitDelay) {
				if text := bytes.TrimSpace(stderr.Bytes()); len(text) > 0 {
					waitErr = fmt.Errorf("%w: %s", waitErr, text)
				}
			}
		})
		return waitErr
	}

	client := NewClient(stdout, stdin, closeProcess)
	initCtx, initCancel := context.WithTimeout(ctx, timeout)
	defer initCancel()
	if err := client.Initialize(initCtx, "codex-feishu-bridge", opts.Version); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("initialize app-server: %w", err)
	}
	return client, nil
}

type ProbeResult struct {
	ThreadCount int
}

func Probe(ctx context.Context, opts ProcessOptions) (ProbeResult, error) {
	client, err := Open(ctx, opts)
	if err != nil {
		return ProbeResult{}, err
	}
	defer client.Close()
	threads, err := client.ListThreads(ctx, 1)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("list desktop threads: %w", err)
	}
	return ProbeResult{ThreadCount: len(threads)}, nil
}
