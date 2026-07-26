package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultCommand               = "codex"
	defaultStartupTimeoutSeconds = 15
	defaultCardDisplayMode       = "preview"
)

type Config struct {
	Feishu    FeishuConfig             `yaml:"feishu"`
	Security  SecurityConfig           `yaml:"security"`
	AppServer AppServerConfig          `yaml:"app_server"`
	Workspace WorkspaceConfig          `yaml:"workspace"`
	Projects  map[string]ProjectConfig `yaml:"projects"`
	Paths     RuntimePaths             `yaml:"paths"`
}

type FeishuConfig struct {
	AppID           string `yaml:"app_id"`
	AppSecretEnv    string `yaml:"app_secret_env"`
	ProxyURL        string `yaml:"proxy_url"`
	CardDisplayMode string `yaml:"card_display_mode"`
}

type SecurityConfig struct {
	AllowedOpenIDs []string `yaml:"allowed_open_ids"`
}

// AppServerConfig configures the local Codex app-server daemon. Execution
// permissions are intentionally not configurable: every bridge turn uses full
// access with approvalPolicy=never.
type AppServerConfig struct {
	Command               string `yaml:"command"`
	DefaultModel          string `yaml:"default_model"`
	StartupTimeoutSeconds int    `yaml:"startup_timeout_seconds"`
}

type WorkspaceConfig struct {
	Default string `yaml:"default"`
}

type ProjectConfig struct {
	CWD   string `yaml:"cwd"`
	Model string `yaml:"model"`
}

type RuntimePaths struct {
	StateDB string `yaml:"state_db"`
}

type ResolvedProject struct {
	Alias string
	CWD   string
	Model string
}

type DiagnosticLevel string

const (
	LevelOK    DiagnosticLevel = "ok"
	LevelWarn  DiagnosticLevel = "warn"
	LevelError DiagnosticLevel = "error"
)

type Diagnostic struct {
	Level   DiagnosticLevel
	Code    string
	Message string
}

func DefaultPath(home string) string {
	return filepath.Join(home, ".codex-feishu-bridge", "config.yaml")
}

func Load(path string, getenv func(string) string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, err
	}
	cfg.applyDefaults(homeDir(getenv))
	return cfg, nil
}

func (cfg Config) Validate(getenv func(string) string, stat func(string) error) []Diagnostic {
	cfg.applyDefaults(homeDir(getenv))
	var diags []Diagnostic
	if cfg.Feishu.AppID == "" {
		diags = append(diags, Diagnostic{Level: LevelError, Code: "feishu.app_id", Message: "Feishu app_id is required"})
	} else {
		diags = append(diags, Diagnostic{Level: LevelOK, Code: "feishu.app_id", Message: "Feishu app_id configured"})
	}
	if cfg.Feishu.AppSecretEnv == "" {
		diags = append(diags, Diagnostic{Level: LevelError, Code: "feishu.app_secret", Message: "Feishu app_secret_env is required"})
	} else if getenv != nil && getenv(cfg.Feishu.AppSecretEnv) == "" {
		diags = append(diags, Diagnostic{Level: LevelError, Code: "feishu.app_secret", Message: fmt.Sprintf("environment variable %s is not set", cfg.Feishu.AppSecretEnv)})
	} else {
		diags = append(diags, Diagnostic{Level: LevelOK, Code: "feishu.app_secret", Message: "Feishu app secret environment variable is set"})
	}
	if strings.TrimSpace(cfg.Feishu.ProxyURL) != "" {
		if _, err := cfg.Feishu.Proxy(); err != nil {
			diags = append(diags, Diagnostic{Level: LevelError, Code: "feishu.proxy_url", Message: err.Error()})
		} else {
			diags = append(diags, Diagnostic{Level: LevelOK, Code: "feishu.proxy_url", Message: "Feishu proxy configured"})
		}
	}
	if !validCardDisplayMode(cfg.Feishu.CardDisplayMode) {
		diags = append(diags, Diagnostic{Level: LevelError, Code: "feishu.card_display_mode", Message: "must be concise or preview"})
	}
	if cfg.Workspace.Default == "" {
		diags = append(diags, Diagnostic{Level: LevelError, Code: "workspace.default", Message: "default workspace is required"})
	} else if stat != nil {
		if err := stat(cfg.Workspace.Default); err != nil {
			diags = append(diags, Diagnostic{Level: LevelError, Code: "workspace.default", Message: fmt.Sprintf("default workspace is not accessible: %v", err)})
		} else {
			diags = append(diags, Diagnostic{Level: LevelOK, Code: "workspace.default", Message: "default workspace exists"})
		}
	}
	for alias, project := range cfg.Projects {
		if project.CWD == "" {
			diags = append(diags, Diagnostic{Level: LevelError, Code: "project." + alias + ".cwd", Message: "project cwd is required"})
		} else if stat != nil {
			if err := stat(project.CWD); err != nil {
				diags = append(diags, Diagnostic{Level: LevelError, Code: "project." + alias + ".cwd", Message: fmt.Sprintf("project cwd is not accessible: %v", err)})
			} else {
				diags = append(diags, Diagnostic{Level: LevelOK, Code: "project." + alias + ".cwd", Message: "project cwd exists"})
			}
		}
	}
	if cfg.AppServer.StartupTimeoutSeconds <= 0 {
		diags = append(diags, Diagnostic{Level: LevelError, Code: "app_server.startup_timeout_seconds", Message: "startup timeout must be positive"})
	}
	return diags
}

// Proxy returns the configured Feishu proxy. A missing value deliberately
// returns nil so all Feishu network traffic uses a direct connection.
func (cfg FeishuConfig) Proxy() (*url.URL, error) {
	rawURL := strings.TrimSpace(cfg.ProxyURL)
	if rawURL == "" {
		return nil, nil
	}
	proxyURL, err := url.ParseRequestURI(rawURL)
	if err != nil || proxyURL.Scheme == "" || proxyURL.Host == "" {
		return nil, errors.New("must be an absolute http URL")
	}
	if strings.ToLower(proxyURL.Scheme) != "http" {
		return nil, errors.New("must use the http scheme")
	}
	return proxyURL, nil
}

func (cfg Config) ResolveProject(alias string) (ResolvedProject, error) {
	cfg.applyDefaults("")
	resolved := ResolvedProject{Alias: alias, CWD: cfg.Workspace.Default, Model: cfg.AppServer.DefaultModel}
	if alias == "" {
		if resolved.CWD == "" {
			return ResolvedProject{}, errors.New("default workspace is not configured")
		}
		return resolved, nil
	}
	project, ok := cfg.Projects[alias]
	if !ok {
		return ResolvedProject{}, fmt.Errorf("unknown project alias %q", alias)
	}
	if project.CWD == "" {
		return ResolvedProject{}, fmt.Errorf("project %q cwd is not configured", alias)
	}
	resolved.CWD = project.CWD
	if project.Model != "" {
		resolved.Model = project.Model
	}
	return resolved, nil
}

func (cfg Config) ProjectAliasForCWD(cwd string) string {
	if cwd == "" {
		return ""
	}
	if samePath(cwd, cfg.Workspace.Default) {
		return ""
	}
	for alias, project := range cfg.Projects {
		if samePath(cwd, project.CWD) {
			return alias
		}
	}
	return ""
}

func (cfg Config) StartupTimeout() time.Duration {
	cfg.applyDefaults("")
	return time.Duration(cfg.AppServer.StartupTimeoutSeconds) * time.Second
}

func (cfg Config) ProjectAliases() []string {
	aliases := make([]string, 0, len(cfg.Projects))
	for alias := range cfg.Projects {
		if alias != "" {
			aliases = append(aliases, alias)
		}
	}
	sort.Strings(aliases)
	return aliases
}

func (cfg *Config) applyDefaults(home string) {
	if cfg.Feishu.CardDisplayMode == "" {
		cfg.Feishu.CardDisplayMode = defaultCardDisplayMode
	}
	if cfg.AppServer.Command == "" {
		cfg.AppServer.Command = defaultCommand
	}
	if cfg.AppServer.StartupTimeoutSeconds == 0 {
		cfg.AppServer.StartupTimeoutSeconds = defaultStartupTimeoutSeconds
	}
	if cfg.Projects == nil {
		cfg.Projects = map[string]ProjectConfig{}
	}
	if home != "" && cfg.Paths.StateDB == "" {
		cfg.Paths.StateDB = filepath.Join(home, ".codex-feishu-bridge", "state.db")
	}
}

func validCardDisplayMode(mode string) bool {
	return mode == "concise" || mode == "preview"
}

func samePath(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	return filepath.Clean(left) == filepath.Clean(right)
}

func homeDir(getenv func(string) string) string {
	if getenv != nil {
		if home := getenv("HOME"); home != "" {
			return home
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return ""
}
