package doctor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sihuo/codex-feishu-bridge/internal/config"
)

func TestCheckDetectsCapabilitiesAndWarnsApprovalOptional(t *testing.T) {
	cfgPath, _ := writeDoctorConfig(t)
	report := Check(context.Background(), Options{
		ConfigPath: cfgPath,
		Getenv: func(key string) string {
			if key == "FEISHU_APP_SECRET" {
				return "secret"
			}
			if key == "HOME" {
				return filepath.Dir(cfgPath)
			}
			return ""
		},
		LookPath: func(command string) (string, error) { return command, nil },
		RunCommand: func(ctx context.Context, command string, args ...string) (string, error) {
			switch strings.Join(args, " ") {
			case "exec --help":
				return "Usage: codex exec --json -C --cd -s --sandbox -m --model resume", nil
			case "exec resume --help":
				return "Usage: codex exec resume <SESSION_ID> <PROMPT>", nil
			default:
				return "", errors.New("unexpected command")
			}
		},
	})
	if report.HasErrors() {
		t.Fatalf("expected no errors, got:\n%s", report.Render())
	}
	if !report.Has(LevelWarn, "codex.approval_flag") {
		t.Fatalf("expected approval warning, got:\n%s", report.Render())
	}
	for _, code := range []string{"codex.command", "codex.exec.json", "codex.exec.resume", "workspace.default", "paths.state_db", "paths.log_dir"} {
		if !report.Has(LevelOK, code) {
			t.Fatalf("missing OK %s in:\n%s", code, report.Render())
		}
	}
}

func TestCheckReportsMissingConfigAndWorkspaceValues(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(`
[feishu]
app_secret_env = "FEISHU_APP_SECRET"

[workspace]
default = "/missing/workspace"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	report := Check(context.Background(), Options{
		ConfigPath: cfgPath,
		Getenv:     func(string) string { return "" },
		LookPath:   func(string) (string, error) { return "", os.ErrNotExist },
		RunCommand: func(context.Context, string, ...string) (string, error) {
			return "", errors.New("should not run")
		},
	})
	for _, code := range []string{"feishu.app_id", "feishu.app_secret", "workspace.default", "codex.command"} {
		if !report.Has(LevelError, code) {
			t.Fatalf("missing FAIL %s in:\n%s", code, report.Render())
		}
	}
	if !strings.Contains(report.Render(), "FAIL feishu.app_id") {
		t.Fatalf("render should include stable FAIL lines:\n%s", report.Render())
	}
}

func TestCheckFailsMissingRequiredCodexFlags(t *testing.T) {
	cfgPath, _ := writeDoctorConfig(t)
	report := Check(context.Background(), Options{
		ConfigPath: cfgPath,
		Getenv: func(key string) string {
			if key == "FEISHU_APP_SECRET" {
				return "secret"
			}
			if key == "HOME" {
				return filepath.Dir(cfgPath)
			}
			return ""
		},
		LookPath: func(command string) (string, error) { return command, nil },
		RunCommand: func(ctx context.Context, command string, args ...string) (string, error) {
			return "Usage: codex exec", nil
		},
	})
	for _, code := range []string{"codex.exec.json", "codex.exec.resume", "codex.exec.cwd", "codex.exec.sandbox", "codex.exec.model"} {
		if !report.Has(LevelError, code) {
			t.Fatalf("missing FAIL %s in:\n%s", code, report.Render())
		}
	}
}

func TestReportRenderAndExitCode(t *testing.T) {
	report := Report{Diagnostics: []Diagnostic{
		{Level: LevelOK, Code: "ok.code", Message: "ok"},
		{Level: LevelWarn, Code: "warn.code", Message: "warn"},
		{Level: LevelError, Code: "fail.code", Message: "fail"},
	}}
	rendered := report.Render()
	for _, line := range []string{"OK ok.code ok", "WARN warn.code warn", "FAIL fail.code fail"} {
		if !strings.Contains(rendered, line) {
			t.Fatalf("missing %q in:\n%s", line, rendered)
		}
	}
	if !report.HasErrors() {
		t.Fatal("expected errors")
	}
}

func writeDoctorConfig(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	workspace := filepath.Join(dir, "workspace")
	project := filepath.Join(dir, "project")
	for _, path := range []string{workspace, project, filepath.Join(dir, "state"), filepath.Join(dir, "logs")} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := filepath.Join(dir, "config.toml")
	content := `
[feishu]
app_id = "cli_test"
app_secret_env = "FEISHU_APP_SECRET"
connection = "websocket"

[security]
allowed_open_ids = ["ou_owner"]

[codex]
command = "codex"

[workspace]
default = "` + workspace + `"

[paths]
state_db = "` + filepath.Join(dir, "state", "state.db") + `"
log_dir = "` + filepath.Join(dir, "logs") + `"

[projects.backend]
cwd = "` + project + `"
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, dir
}

func TestToConfigDiagnostic(t *testing.T) {
	got := fromConfigDiagnostic(config.Diagnostic{Level: config.LevelError, Code: "x", Message: "y"})
	if got.Level != LevelError || got.Code != "x" || got.Message != "y" {
		t.Fatalf("unexpected conversion: %+v", got)
	}
}
