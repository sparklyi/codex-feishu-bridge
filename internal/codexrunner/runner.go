package codexrunner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/sparklyi/codex-feishu-bridge/internal/contracts"
	"github.com/sparklyi/codex-feishu-bridge/internal/logs"
)

const defaultStderrLimit = 8 * 1024

type Runner struct {
	LogDir      string
	StderrLimit int
	Now         func() time.Time
}

type ExecInput struct {
	Command               string
	CWD                   string
	Sandbox               string
	Model                 string
	Approval              string
	ApprovalFlagSupported bool
	ExtraArgs             []string
	Prompt                string
	TaskID                string
	RunID                 string
	OnSessionID           func(string) error
}

type ResumeInput struct {
	ExecInput
	SessionID string
	Reply     string
}

type RunError struct {
	Kind     string
	ExitCode int
	Message  string
}

func (e *RunError) Error() string {
	if e == nil {
		return ""
	}
	if e.ExitCode != 0 {
		return fmt.Sprintf("%s: exit %d: %s", e.Kind, e.ExitCode, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Kind, e.Message)
}

func (r *Runner) Exec(ctx context.Context, in ExecInput) (contracts.RunResult, error) {
	args := buildArgs(in, []string{in.Prompt})
	return r.run(ctx, in, args)
}

func (r *Runner) Resume(ctx context.Context, in ResumeInput) (contracts.RunResult, error) {
	args := buildArgs(in.ExecInput, []string{"resume", in.SessionID, in.Reply})
	if in.Prompt == "" {
		in.Prompt = in.Reply
	}
	return r.run(ctx, in.ExecInput, args)
}

func buildArgs(in ExecInput, tail []string) []string {
	args := []string{"exec", "--json", "-C", in.CWD, "-s", in.Sandbox}
	if in.Model != "" {
		args = append(args, "-m", in.Model)
	}
	if in.ApprovalFlagSupported && in.Approval != "" {
		args = append(args, "-a", in.Approval)
	}
	args = append(args, in.ExtraArgs...)
	args = append(args, tail...)
	return args
}

func (r *Runner) run(ctx context.Context, in ExecInput, args []string) (contracts.RunResult, error) {
	if _, err := exec.LookPath(in.Command); err != nil {
		return contracts.RunResult{}, &RunError{Kind: "command_not_found", Message: err.Error()}
	}
	logPath, logWriter, err := logs.OpenRunLogWriter(r.LogDir, in.TaskID, in.RunID)
	if err != nil {
		return contracts.RunResult{}, err
	}
	defer logWriter.Close()

	now := r.now
	startedAt := now()
	cmd := exec.CommandContext(ctx, in.Command, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return contracts.RunResult{}, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return contracts.RunResult{LogPath: logPath, StartedAt: startedAt, FinishedAt: now()}, &RunError{Kind: "canceled", Message: ctx.Err().Error()}
		}
		return contracts.RunResult{}, &RunError{Kind: "command_not_found", Message: err.Error()}
	}

	parseCallbacks := ParserCallbacks{}
	if in.OnSessionID != nil {
		parseCallbacks.OnSessionID = func(threadID string) error {
			if err := in.OnSessionID(threadID); err != nil {
				return sessionCallbackError{err: err}
			}
			return nil
		}
	}
	parsed, parseErr := ParseJSONLStream(io.TeeReader(stdout, logWriter), parseCallbacks)
	if parseErr != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	finishedAt := now()
	exitCode := exitCode(waitErr)
	result := contracts.RunResult{
		CodexSessionID: parsed.CodexSessionID,
		FinalText:      parsed.FinalText,
		ExitCode:       exitCode,
		StderrTail:     tailString(stderr.String(), r.stderrLimit()),
		LogPath:        logPath,
		StartedAt:      startedAt,
		FinishedAt:     finishedAt,
	}
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return result, &RunError{Kind: "canceled", Message: ctx.Err().Error()}
	}
	if parseErr != nil {
		var callbackErr sessionCallbackError
		if errors.As(parseErr, &callbackErr) {
			return result, &RunError{Kind: "session_callback", Message: callbackErr.err.Error()}
		}
		return result, &RunError{Kind: "parse", Message: parseErr.Error()}
	}
	if waitErr != nil {
		return result, &RunError{Kind: "non_zero_exit", ExitCode: exitCode, Message: result.StderrTail}
	}
	return result, nil
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now().UTC()
}

func (r *Runner) stderrLimit() int {
	if r.StderrLimit > 0 {
		return r.StderrLimit
	}
	return defaultStderrLimit
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func tailString(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	return s[len(s)-limit:]
}

type sessionCallbackError struct {
	err error
}

func (e sessionCallbackError) Error() string {
	return e.err.Error()
}

func (e sessionCallbackError) Unwrap() error {
	return e.err
}
