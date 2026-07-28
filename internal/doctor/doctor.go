package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/sparklyi/codex-feishu-bridge/internal/appserver"
	"github.com/sparklyi/codex-feishu-bridge/internal/config"
	"github.com/sparklyi/codex-feishu-bridge/internal/store"
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
	ConfigPath  string
	Getenv      func(string) string
	Stat        func(string) error
	LookPath    func(string) (string, error)
	Probe       func(context.Context, appserver.ProcessOptions) (appserver.ProbeResult, error)
	Version     func(context.Context, string) (string, error)
	CheckSchema func(context.Context, string) error
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
	diags := []Diagnostic{{Level: LevelOK, Code: "config.load", Message: "config parsed"}}
	for _, diag := range cfg.Validate(opts.Getenv, opts.Stat) {
		diags = append(diags, fromConfigDiagnostic(diag))
	}
	diags = append(diags, checkSQLite(ctx, cfg.Paths.StateDB)...)
	diags = append(diags, checkAppServer(ctx, cfg, opts)...)
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
	if opts.Probe == nil {
		opts.Probe = appserver.Probe
	}
	if opts.Version == nil {
		opts.Version = appserver.Version
	}
	if opts.CheckSchema == nil {
		opts.CheckSchema = appserver.CheckSchema
	}
	return opts
}

func fromConfigDiagnostic(diag config.Diagnostic) Diagnostic {
	return Diagnostic{Level: Level(diag.Level), Code: diag.Code, Message: diag.Message}
}

func checkAppServer(ctx context.Context, cfg config.Config, opts Options) []Diagnostic {
	command := cfg.AppServer.Command
	if command == "" {
		command = "codex"
	}
	resolved, err := opts.LookPath(command)
	if err != nil {
		return []Diagnostic{{Level: LevelError, Code: "app_server.command", Message: fmt.Sprintf("%s not found: %v", command, err)}}
	}
	diags := []Diagnostic{{Level: LevelOK, Code: "app_server.command", Message: "Codex command found: " + resolved}}
	versionCtx, cancelVersion := context.WithTimeout(ctx, cfg.StartupTimeout())
	version, err := opts.Version(versionCtx, command)
	cancelVersion()
	if err != nil {
		diags = append(diags, Diagnostic{Level: LevelWarn, Code: "app_server.version", Message: err.Error()})
	} else {
		diags = append(diags, Diagnostic{Level: LevelOK, Code: "app_server.version", Message: version})
	}
	schemaCtx, cancelSchema := context.WithTimeout(ctx, cfg.StartupTimeout())
	err = opts.CheckSchema(schemaCtx, command)
	cancelSchema()
	if err != nil {
		level := LevelError
		if errors.Is(err, appserver.ErrSchemaGeneratorUnavailable) {
			level = LevelWarn
		}
		diags = append(diags, Diagnostic{Level: level, Code: "app_server.schema", Message: err.Error()})
	} else {
		diags = append(diags, Diagnostic{Level: LevelOK, Code: "app_server.schema", Message: "bridge app-server request contracts are compatible"})
	}
	probeCtx, cancel := context.WithTimeout(ctx, cfg.StartupTimeout())
	defer cancel()
	result, err := opts.Probe(probeCtx, appserver.ProcessOptions{
		Command:         command,
		Version:         "doctor",
		Timeout:         cfg.StartupTimeout(),
		ExperimentalAPI: cfg.AppServer.ExperimentalAPI,
	})
	if err != nil {
		return append(diags, Diagnostic{Level: LevelError, Code: "app_server.probe", Message: err.Error()})
	}
	return append(diags, Diagnostic{Level: LevelOK, Code: "app_server.probe", Message: fmt.Sprintf("app-server handshake succeeded; %d desktop threads visible", result.ThreadCount)})
}

func checkSQLite(ctx context.Context, path string) []Diagnostic {
	if path == "" {
		return []Diagnostic{{Level: LevelError, Code: "paths.state_db", Message: "path is empty"}}
	}
	s, err := store.Open(ctx, path)
	if err != nil {
		return []Diagnostic{{Level: LevelError, Code: "paths.state_db", Message: err.Error()}}
	}
	if err := s.Close(); err != nil {
		return []Diagnostic{{Level: LevelError, Code: "paths.state_db", Message: err.Error()}}
	}
	return []Diagnostic{{Level: LevelOK, Code: "paths.state_db", Message: "SQLite database migrated and writable"}}
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
