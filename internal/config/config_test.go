package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestResolveProjectDefaults(t *testing.T) {
	cfg := Config{
		Codex:     CodexConfig{Command: "codex", DefaultModel: "gpt-5.5", Sandbox: "workspace-write", Approval: "never", ExtraArgs: []string{"--skip-git-repo-check"}},
		Workspace: WorkspaceConfig{Default: "/repo/default"},
		Projects: map[string]ProjectConfig{
			"backend": {CWD: "/repo/backend", Model: "gpt-5.5", Sandbox: "read-only"},
		},
	}
	got, err := cfg.ResolveProject("backend")
	if err != nil {
		t.Fatal(err)
	}
	if got.CWD != "/repo/backend" || got.Model != "gpt-5.5" || got.Sandbox != "read-only" || got.Approval != "never" || len(got.ExtraArgs) != 1 {
		t.Fatalf("unexpected resolved project: %+v", got)
	}
	if got.Command != "codex" || got.LogRetentionDays != 14 {
		t.Fatalf("missing runtime values: %+v", got)
	}
}

func TestResolveUnknownProject(t *testing.T) {
	cfg := Config{Workspace: WorkspaceConfig{Default: "/repo/default"}}
	if _, err := cfg.ResolveProject("missing"); err == nil {
		t.Fatal("expected unknown project error")
	}
}

func TestResolveDefaultWorkspace(t *testing.T) {
	cfg := Config{
		Codex:     CodexConfig{DefaultModel: "gpt-5", Sandbox: "workspace-write", Approval: "never"},
		Workspace: WorkspaceConfig{Default: "/repo/default"},
	}
	got, err := cfg.ResolveProject("")
	if err != nil {
		t.Fatal(err)
	}
	if got.Alias != "" || got.CWD != "/repo/default" || got.Model != "gpt-5" || got.Command != "codex" {
		t.Fatalf("unexpected default resolution: %+v", got)
	}
}

func TestProjectApprovalFallbackAndOverride(t *testing.T) {
	cfg := Config{
		Codex:     CodexConfig{Approval: "never"},
		Workspace: WorkspaceConfig{Default: "/repo/default"},
		Projects: map[string]ProjectConfig{
			"fallback": {CWD: "/repo/fallback"},
			"override": {CWD: "/repo/override", Approval: "on-request"},
		},
	}
	got, err := cfg.ResolveProject("fallback")
	if err != nil {
		t.Fatal(err)
	}
	if got.Approval != "never" {
		t.Fatalf("expected fallback approval, got %q", got.Approval)
	}
	got, err = cfg.ResolveProject("override")
	if err != nil {
		t.Fatal(err)
	}
	if got.Approval != "on-request" {
		t.Fatalf("expected override approval, got %q", got.Approval)
	}
}

func TestLoadYAMLAndValidate(t *testing.T) {
	dir := t.TempDir()
	workspace := filepath.Join(dir, "workspace")
	project := filepath.Join(dir, "backend")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	yaml := `
feishu:
  app_id: cli_test
  app_secret_env: FEISHU_APP_SECRET
  connection: websocket
security:
  allowed_open_ids:
    - ou_owner
codex:
  extra_args:
    - --skip-git-repo-check
workspace:
  default: "` + strings.ReplaceAll(workspace, `\`, `\\`) + `"
projects:
  backend:
    cwd: "` + strings.ReplaceAll(project, `\`, `\\`) + `"
    sandbox: read-only
    approval: on-request
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path, func(key string) string {
		if key == "FEISHU_APP_SECRET" {
			return "secret"
		}
		if key == "HOME" {
			return dir
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := cfg.ResolveProject("backend")
	if err != nil {
		t.Fatal(err)
	}
	if got.Command != "codex" || got.Sandbox != "read-only" || got.Approval != "on-request" || got.LogRetentionDays != 14 {
		t.Fatalf("defaults not applied: %+v", got)
	}
	if cfg.Paths.StateDB != filepath.Join(dir, ".codex-feishu-bridge", "state.db") {
		t.Fatalf("unexpected state path: %q", cfg.Paths.StateDB)
	}
	diags := cfg.Validate(func(key string) string {
		if key == "FEISHU_APP_SECRET" {
			return "secret"
		}
		return ""
	}, func(path string) error {
		_, err := os.Stat(path)
		return err
	})
	if hasError(diags) {
		t.Fatalf("expected no validation errors, got %+v", diags)
	}
}

func TestLoadFeishuBotOpenID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	err := os.WriteFile(path, []byte(`
feishu:
  app_id: cli_test
  app_secret_env: FEISHU_APP_SECRET
  bot_open_id: ou_bot
workspace:
  default: /repo/default
projects:
  backend:
    cwd: /repo/backend
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path, func(key string) string {
		if key == "HOME" {
			return dir
		}
		if key == "FEISHU_APP_SECRET" {
			return "secret"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Feishu.BotOpenID != "ou_bot" {
		t.Fatalf("bot open id = %q", cfg.Feishu.BotOpenID)
	}
}

func TestProjectAliasesSorted(t *testing.T) {
	cfg := Config{Projects: map[string]ProjectConfig{
		"frontend": {CWD: "/repo/frontend"},
		"backend":  {CWD: "/repo/backend"},
	}}
	got := cfg.ProjectAliases()
	want := []string{"backend", "frontend"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ProjectAliases() = %v, want %v", got, want)
	}
}

func TestValidateReportsMissingRequiredValues(t *testing.T) {
	cfg := Config{}
	diags := cfg.Validate(func(string) string { return "" }, func(path string) error { return os.ErrNotExist })
	for _, code := range []string{"feishu.app_id", "feishu.app_secret", "workspace.default"} {
		if !hasDiagnostic(diags, LevelError, code) {
			t.Fatalf("missing diagnostic %s in %+v", code, diags)
		}
	}
}

func TestDefaultPath(t *testing.T) {
	got := DefaultPath("/home/alice")
	want := filepath.Join("/home/alice", ".codex-feishu-bridge", "config.yaml")
	if got != want {
		t.Fatalf("DefaultPath() = %q, want %q", got, want)
	}
}

func TestExampleConfigParses(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "config.example.yaml"), func(key string) string {
		if key == "HOME" {
			return t.TempDir()
		}
		if key == "FEISHU_APP_SECRET" {
			return "secret"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Feishu.Connection != "websocket" || cfg.Codex.Sandbox != "workspace-write" {
		t.Fatalf("unexpected example config: %+v", cfg)
	}
}

func hasError(diags []Diagnostic) bool {
	for _, diag := range diags {
		if diag.Level == LevelError {
			return true
		}
	}
	return false
}

func hasDiagnostic(diags []Diagnostic, level DiagnosticLevel, code string) bool {
	for _, diag := range diags {
		if diag.Level == level && diag.Code == code {
			return true
		}
	}
	return false
}
