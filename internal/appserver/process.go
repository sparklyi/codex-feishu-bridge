package appserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
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
			killed := false
			if cmd.Process != nil {
				if err := cmd.Process.Kill(); err == nil {
					killed = true
				} else if !errors.Is(err, os.ErrProcessDone) {
					waitErr = err
				}
			}
			if err := cmd.Wait(); err != nil && !killed && waitErr == nil {
				waitErr = err
			}
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
		if closeErr := client.Close(); closeErr != nil {
			return nil, fmt.Errorf("initialize app-server: %w", errors.Join(err, fmt.Errorf("close app-server: %w", closeErr)))
		}
		return nil, fmt.Errorf("initialize app-server: %w", err)
	}
	return client, nil
}

type ProbeResult struct {
	ThreadCount int
}

func Probe(ctx context.Context, opts ProcessOptions) (result ProbeResult, err error) {
	client, err := Open(ctx, opts)
	if err != nil {
		return ProbeResult{}, err
	}
	defer func() {
		if closeErr := client.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close app-server: %w", closeErr)
		}
	}()
	threads, err := client.ListThreads(ctx, 1)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("list desktop threads: %w", err)
	}
	return ProbeResult{ThreadCount: len(threads)}, nil
}
