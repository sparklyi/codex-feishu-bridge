package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestResolveProjectDefaultsAndOverrides(t *testing.T) {
	cfg := Config{
		AppServer: AppServerConfig{DefaultModel: "gpt-5"},
		Workspace: WorkspaceConfig{Default: "/repo/default"},
		Projects: map[string]ProjectConfig{
			"backend": {CWD: "/repo/backend", Model: "gpt-5.1"},
		},
	}
	got, err := cfg.ResolveProject("backend")
	if err != nil {
		t.Fatal(err)
	}
	if got.Alias != "backend" || got.CWD != "/repo/backend" || got.Model != "gpt-5.1" {
		t.Fatalf("unexpected resolved project: %+v", got)
	}
	defaultProject, err := cfg.ResolveProject("")
	if err != nil {
		t.Fatal(err)
	}
	if defaultProject.CWD != "/repo/default" || defaultProject.Model != "gpt-5" {
		t.Fatalf("unexpected default project: %+v", defaultProject)
	}
}

func TestLoadRejectsPermissionConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("app_server:\n  sandbox: workspace-write\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, func(string) string { return t.TempDir() }); err == nil || !strings.Contains(err.Error(), "sandbox") {
		t.Fatalf("permission configuration must be rejected, got %v", err)
	}
}

func TestLoadYAMLValidatesAppServerAndRejectsLegacyCodexConfig(t *testing.T) {
	dir := t.TempDir()
	workspace := filepath.Join(dir, "workspace")
	project := filepath.Join(dir, "backend")
	for _, path := range []string{workspace, project} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "config.yaml")
	yaml := `
feishu:
  app_id: cli_test
  app_secret_env: FEISHU_APP_SECRET
security:
  allowed_open_ids: [ou_owner]
app_server:
  command: /usr/local/bin/codex
  startup_timeout_seconds: 9
workspace:
  default: "` + workspace + `"
projects:
  backend:
    cwd: "` + project + `"
paths:
  state_db: "` + filepath.Join(dir, "state.db") + `"
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path, func(key string) string {
		switch key {
		case "HOME":
			return dir
		case "FEISHU_APP_SECRET":
			return "secret"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AppServer.Command != "/usr/local/bin/codex" || cfg.StartupTimeout() != 9*time.Second {
		t.Fatalf("app-server config not loaded: %+v", cfg.AppServer)
	}
	if hasError(cfg.Validate(func(key string) string {
		if key == "FEISHU_APP_SECRET" {
			return "secret"
		}
		return ""
	}, func(path string) error {
		_, err := os.Stat(path)
		return err
	})) {
		t.Fatal("expected valid config")
	}

	legacy := filepath.Join(dir, "legacy.yaml")
	if err := os.WriteFile(legacy, []byte("codex:\n  command: codex\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(legacy, func(string) string { return dir }); err == nil || !strings.Contains(err.Error(), "codex") {
		t.Fatalf("legacy codex config must fail, got %v", err)
	}
}

func TestProjectAliasForCWDAndAliases(t *testing.T) {
	cfg := Config{
		Workspace: WorkspaceConfig{Default: "/repo/default"},
		Projects: map[string]ProjectConfig{
			"frontend": {CWD: "/repo/frontend"},
			"backend":  {CWD: "/repo/backend"},
		},
	}
	if got := cfg.ProjectAliasForCWD("/repo/default/"); got != "" {
		t.Fatalf("default alias = %q", got)
	}
	if got := cfg.ProjectAliasForCWD("/repo/backend"); got != "backend" {
		t.Fatalf("backend alias = %q", got)
	}
	if got := cfg.ProjectAliasForCWD("/outside"); got != "" {
		t.Fatalf("unknown alias = %q", got)
	}
	if got, want := cfg.ProjectAliases(), []string{"backend", "frontend"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ProjectAliases() = %v, want %v", got, want)
	}
}

func TestLoadRejectsRetiredFeishuFields(t *testing.T) {
	for _, field := range []string{"bot_open_id: ou_bot", "connection: websocket"} {
		t.Run(field, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			data := "feishu:\n  app_id: cli_test\n  app_secret_env: FEISHU_APP_SECRET\n  " + field + "\n"
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			fieldName := strings.Split(field, ":")[0]
			if _, err := Load(path, func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), fieldName) {
				t.Fatalf("retired field %q must be rejected, got %v", field, err)
			}
		})
	}
}

func TestFeishuProxyURL(t *testing.T) {
	cases := []struct {
		name    string
		rawURL  string
		wantURL string
		wantErr bool
	}{
		{name: "direct by default"},
		{name: "http proxy", rawURL: "http://127.0.0.1:7890", wantURL: "http://127.0.0.1:7890"},
		{name: "missing scheme", rawURL: "proxy.example.test:7890", wantErr: true},
		{name: "https proxy", rawURL: "https://proxy.example.test:8443", wantErr: true},
		{name: "unsupported scheme", rawURL: "socks5://127.0.0.1:1080", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proxyURL, err := (FeishuConfig{ProxyURL: tc.rawURL}).Proxy()
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected proxy URL error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantURL == "" {
				if proxyURL != nil {
					t.Fatalf("proxy URL = %q, want nil", proxyURL)
				}
				return
			}
			if proxyURL == nil || proxyURL.String() != tc.wantURL {
				t.Fatalf("proxy URL = %v, want %q", proxyURL, tc.wantURL)
			}
		})
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
	diags := (Config{}).Validate(func(string) string { return "" }, func(string) error { return os.ErrNotExist })
	for _, code := range []string{"feishu.app_id", "feishu.app_secret", "workspace.default"} {
		if !hasDiagnostic(diags, LevelError, code) {
			t.Fatalf("missing diagnostic %s in %+v", code, diags)
		}
	}
}

func TestDefaultPathAndExampleConfig(t *testing.T) {
	if got, want := DefaultPath("/home/alice"), filepath.Join("/home/alice", ".codex-feishu-bridge", "config.yaml"); got != want {
		t.Fatalf("DefaultPath() = %q, want %q", got, want)
	}
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
	if cfg.AppServer.Command != "codex" || cfg.StartupTimeout() != 15*time.Second {
		t.Fatalf("unexpected example config: %+v", cfg.AppServer)
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
