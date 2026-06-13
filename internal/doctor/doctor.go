package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sihuo/codex-feishu-bridge/internal/config"
)

type Level = config.DiagnosticLevel

const (
	LevelOK    = config.LevelOK
	LevelWarn  = config.LevelWarn
	LevelError = config.LevelError
)

type Diagnostic struct {
	Level   Level
	Code    string
	Message string
}

type Report struct {
	Diagnostics []Diagnostic
}

type Options struct {
	ConfigPath string
	Getenv     func(string) string
	Stat       func(string) error
	LookPath   func(string) (string, error)
	RunCommand func(context.Context, string, ...string) (string, error)
}

func Check(ctx context.Context, opts Options) Report {
	opts = opts.withDefaults()
	cfgPath := opts.ConfigPath
	if cfgPath == "" {
		cfgPath = config.DefaultPath(opts.Getenv("HOME"))
	}
	cfg, err := config.Load(cfgPath, opts.Getenv)
	if err != nil {
		return Report{Diagnostics: []Diagnostic{{Level: LevelError, Code: "config.load", Message: err.Error()}}}
	}
	var diags []Diagnostic
	diags = append(diags, Diagnostic{Level: LevelOK, Code: "config.load", Message: "config parsed"})
	for _, diag := range cfg.Validate(opts.Getenv, opts.Stat) {
		diags = append(diags, fromConfigDiagnostic(diag))
	}
	diags = append(diags, checkWritableFileParent("paths.state_db", cfg.Paths.StateDB)...)
	diags = append(diags, checkWritableDir("paths.log_dir", cfg.Paths.LogDir)...)
	diags = append(diags, checkCodex(ctx, cfg.Codex.Command, opts)...)
	return Report{Diagnostics: diags}
}

func (opts Options) withDefaults() Options {
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}
	if opts.Stat == nil {
		opts.Stat = func(path string) error {
			_, err := os.Stat(path)
			return err
		}
	}
	if opts.LookPath == nil {
		opts.LookPath = exec.LookPath
	}
	if opts.RunCommand == nil {
		opts.RunCommand = func(ctx context.Context, command string, args ...string) (string, error) {
			cmd := exec.CommandContext(ctx, command, args...)
			out, err := cmd.CombinedOutput()
			return string(out), err
		}
	}
	return opts
}

func fromConfigDiagnostic(diag config.Diagnostic) Diagnostic {
	return Diagnostic{Level: Level(diag.Level), Code: diag.Code, Message: diag.Message}
}

func checkCodex(ctx context.Context, command string, opts Options) []Diagnostic {
	if command == "" {
		command = "codex"
	}
	var diags []Diagnostic
	resolved, err := opts.LookPath(command)
	if err != nil {
		return append(diags, Diagnostic{Level: LevelError, Code: "codex.command", Message: fmt.Sprintf("%s not found: %v", command, err)})
	}
	diags = append(diags, Diagnostic{Level: LevelOK, Code: "codex.command", Message: "Codex command found: " + resolved})
	help, err := opts.RunCommand(ctx, command, "exec", "--help")
	if err != nil {
		diags = append(diags, Diagnostic{Level: LevelError, Code: "codex.exec.help", Message: err.Error()})
	}
	resumeHelp, resumeErr := opts.RunCommand(ctx, command, "exec", "resume", "--help")
	if resumeErr != nil {
		diags = append(diags, Diagnostic{Level: LevelError, Code: "codex.exec.resume_help", Message: resumeErr.Error()})
	} else if strings.Contains(strings.ToLower(resumeHelp), "session") && strings.Contains(strings.ToLower(resumeHelp), "prompt") {
		diags = append(diags, Diagnostic{Level: LevelOK, Code: "codex.exec.resume_help", Message: "resume help accepts session id and prompt"})
	} else {
		diags = append(diags, Diagnostic{Level: LevelWarn, Code: "codex.exec.resume_help", Message: "resume help did not explicitly show session id and prompt"})
	}
	diags = append(diags,
		flagDiagnostic(help, "codex.exec.json", "exec --json support", "--json"),
		flagDiagnostic(help, "codex.exec.resume", "exec resume support", "resume"),
		flagDiagnostic(help, "codex.exec.cwd", "exec cwd flag support", "-C", "--cd"),
		flagDiagnostic(help, "codex.exec.sandbox", "exec sandbox flag support", "-s", "--sandbox"),
		flagDiagnostic(help, "codex.exec.model", "exec model flag support", "-m", "--model"),
	)
	if containsAny(help, "-a", "--approval") {
		diags = append(diags, Diagnostic{Level: LevelOK, Code: "codex.approval_flag", Message: "approval flag supported"})
	} else {
		diags = append(diags, Diagnostic{Level: LevelWarn, Code: "codex.approval_flag", Message: "approval flag not supported; runtime will omit it"})
	}
	return diags
}

func flagDiagnostic(help, code, label string, needles ...string) Diagnostic {
	if containsAny(help, needles...) {
		return Diagnostic{Level: LevelOK, Code: code, Message: label}
	}
	return Diagnostic{Level: LevelError, Code: code, Message: label + " missing"}
}

func containsAny(text string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func checkWritableFileParent(code, path string) []Diagnostic {
	if path == "" {
		return []Diagnostic{{Level: LevelError, Code: code, Message: "path is empty"}}
	}
	return checkWritableDir(code, filepath.Dir(path))
}

func checkWritableDir(code, dir string) []Diagnostic {
	if dir == "" {
		return []Diagnostic{{Level: LevelError, Code: code, Message: "directory is empty"}}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return []Diagnostic{{Level: LevelError, Code: code, Message: err.Error()}}
	}
	file, err := os.CreateTemp(dir, ".doctor-*")
	if err != nil {
		return []Diagnostic{{Level: LevelError, Code: code, Message: err.Error()}}
	}
	name := file.Name()
	closeErr := file.Close()
	removeErr := os.Remove(name)
	if closeErr != nil {
		return []Diagnostic{{Level: LevelError, Code: code, Message: closeErr.Error()}}
	}
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return []Diagnostic{{Level: LevelError, Code: code, Message: removeErr.Error()}}
	}
	return []Diagnostic{{Level: LevelOK, Code: code, Message: "writable"}}
}

func (r Report) HasErrors() bool {
	for _, diag := range r.Diagnostics {
		if diag.Level == LevelError {
			return true
		}
	}
	return false
}

func (r Report) Has(level Level, code string) bool {
	for _, diag := range r.Diagnostics {
		if diag.Level == level && diag.Code == code {
			return true
		}
	}
	return false
}

func (r Report) Render() string {
	var b strings.Builder
	for _, diag := range r.Diagnostics {
		fmt.Fprintf(&b, "%s %s %s\n", renderLevel(diag.Level), diag.Code, diag.Message)
	}
	return b.String()
}

func renderLevel(level Level) string {
	switch level {
	case LevelOK:
		return "OK"
	case LevelWarn:
		return "WARN"
	default:
		return "FAIL"
	}
}
