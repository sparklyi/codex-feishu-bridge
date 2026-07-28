package doctor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sparklyi/codex-feishu-bridge/internal/appserver"
	"github.com/sparklyi/codex-feishu-bridge/internal/config"
)

func TestCheckProbesManagedAppServerAndSQLite(t *testing.T) {
	cfgPath, dir := writeDoctorConfig(t)
	var called appserver.ProcessOptions
	report := Check(context.Background(), Options{
		ConfigPath: cfgPath,
		Getenv:     doctorEnv(dir),
		LookPath:   func(command string) (string, error) { return "/bin/" + command, nil },
		Probe: func(_ context.Context, opts appserver.ProcessOptions) (appserver.ProbeResult, error) {
			called = opts
			return appserver.ProbeResult{ThreadCount: 3}, nil
		},
		Version:     func(context.Context, string) (string, error) { return "codex-cli 0.145.0", nil },
		CheckSchema: func(context.Context, string) error { return nil },
	})
	if report.HasErrors() {
		t.Fatalf("expected no errors:\n%s", report.Render())
	}
	for _, code := range []string{"config.load", "workspace.default", "paths.state_db", "app_server.command", "app_server.version", "app_server.schema", "app_server.probe"} {
		if !report.Has(LevelOK, code) {
			t.Fatalf("missing OK %s in:\n%s", code, report.Render())
		}
	}
	if called.Command != "codex" || called.Timeout <= 0 || called.ExperimentalAPI {
		t.Fatalf("unexpected probe options: %+v", called)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "state", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var name string
	if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='tasks'`).Scan(&name); err != nil {
		t.Fatalf("doctor did not run migrations: %v", err)
	}
}

func TestCheckReportsMissingCommandAndProbeFailure(t *testing.T) {
	cfgPath, dir := writeDoctorConfig(t)
	missing := Check(context.Background(), Options{
		ConfigPath: cfgPath,
		Getenv:     doctorEnv(dir),
		LookPath:   func(string) (string, error) { return "", os.ErrNotExist },
		Probe: func(context.Context, appserver.ProcessOptions) (appserver.ProbeResult, error) {
			return appserver.ProbeResult{}, errors.New("must not run")
		},
	})
	if !missing.Has(LevelError, "app_server.command") {
		t.Fatalf("expected command error:\n%s", missing.Render())
	}

	failedProbe := Check(context.Background(), Options{
		ConfigPath: cfgPath,
		Getenv:     doctorEnv(dir),
		LookPath:   func(command string) (string, error) { return command, nil },
		Probe: func(context.Context, appserver.ProcessOptions) (appserver.ProbeResult, error) {
			return appserver.ProbeResult{}, errors.New("daemon unavailable")
		},
		Version:     func(context.Context, string) (string, error) { return "codex-cli 0.145.0", nil },
		CheckSchema: func(context.Context, string) error { return nil },
	})
	if !failedProbe.Has(LevelError, "app_server.probe") || !strings.Contains(failedProbe.Render(), "daemon unavailable") {
		t.Fatalf("expected probe error:\n%s", failedProbe.Render())
	}
}

func TestCheckReportsSchemaCompatibilityDiagnostics(t *testing.T) {
	cfgPath, dir := writeDoctorConfig(t)
	base := Options{
		ConfigPath: cfgPath,
		Getenv:     doctorEnv(dir),
		LookPath:   func(command string) (string, error) { return command, nil },
		Probe: func(context.Context, appserver.ProcessOptions) (appserver.ProbeResult, error) {
			return appserver.ProbeResult{}, nil
		},
		Version: func(context.Context, string) (string, error) { return "codex-cli 0.145.0", nil },
	}

	missingGenerator := base
	missingGenerator.CheckSchema = func(context.Context, string) error {
		return fmt.Errorf("%w: command not found", appserver.ErrSchemaGeneratorUnavailable)
	}
	report := Check(context.Background(), missingGenerator)
	if !report.Has(LevelWarn, "app_server.schema") || report.HasErrors() {
		t.Fatalf("schema generator warning report:\n%s", report.Render())
	}

	incompatible := base
	incompatible.CheckSchema = func(context.Context, string) error { return errors.New("turn/start missing sandboxPolicy") }
	report = Check(context.Background(), incompatible)
	if !report.Has(LevelError, "app_server.schema") {
		t.Fatalf("schema incompatibility report:\n%s", report.Render())
	}
}

func TestCheckReportsConfigErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("feishu:\n  app_secret_env: FEISHU_APP_SECRET\nworkspace:\n  default: /missing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	report := Check(context.Background(), Options{ConfigPath: path, Getenv: func(string) string { return "" }, LookPath: func(string) (string, error) { return "", os.ErrNotExist }})
	for _, code := range []string{"feishu.app_id", "feishu.app_secret", "workspace.default", "app_server.command"} {
		if !report.Has(LevelError, code) {
			t.Fatalf("missing FAIL %s in:\n%s", code, report.Render())
		}
	}
}

func TestReportRenderAndConfigDiagnostic(t *testing.T) {
	report := Report{Diagnostics: []Diagnostic{{Level: LevelOK, Code: "ok", Message: "ok"}, {Level: LevelWarn, Code: "warn", Message: "warn"}, {Level: LevelError, Code: "fail", Message: "fail"}}}
	for _, line := range []string{"OK ok ok", "WARN warn warn", "FAIL fail fail"} {
		if !strings.Contains(report.Render(), line) {
			t.Fatalf("missing %q", line)
		}
	}
	got := fromConfigDiagnostic(config.Diagnostic{Level: config.LevelError, Code: "x", Message: "y"})
	if got.Level != LevelError || got.Code != "x" || got.Message != "y" {
		t.Fatalf("unexpected conversion: %+v", got)
	}
}

func writeDoctorConfig(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	workspace := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	content := `
feishu:
  app_id: cli_test
  app_secret_env: FEISHU_APP_SECRET
security:
  allowed_open_ids: [ou_owner]
app_server:
  command: codex
workspace:
  default: "` + workspace + `"
paths:
  state_db: "` + filepath.Join(dir, "state", "state.db") + `"
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, dir
}

func doctorEnv(dir string) func(string) string {
	return func(key string) string {
		switch key {
		case "FEISHU_APP_SECRET":
			return "secret"
		case "HOME":
			return dir
		}
		return ""
	}
}
