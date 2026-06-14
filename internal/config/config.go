package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

const (
	defaultCommand          = "codex"
	defaultConnection       = "websocket"
	defaultSandbox          = "workspace-write"
	defaultApproval         = "never"
	defaultLogRetentionDays = 14
)

type Config struct {
	Feishu    FeishuConfig             `yaml:"feishu"`
	Security  SecurityConfig           `yaml:"security"`
	Codex     CodexConfig              `yaml:"codex"`
	Workspace WorkspaceConfig          `yaml:"workspace"`
	Projects  map[string]ProjectConfig `yaml:"projects"`
	Paths     RuntimePaths             `yaml:"paths"`
}

type FeishuConfig struct {
	AppID        string `yaml:"app_id"`
	AppSecretEnv string `yaml:"app_secret_env"`
	Connection   string `yaml:"connection"`
}

type SecurityConfig struct {
	AllowedOpenIDs []string `yaml:"allowed_open_ids"`
}

type CodexConfig struct {
	Command          string   `yaml:"command"`
	DefaultModel     string   `yaml:"default_model"`
	Sandbox          string   `yaml:"sandbox"`
	Approval         string   `yaml:"approval"`
	ExtraArgs        []string `yaml:"extra_args"`
	LogRetentionDays int      `yaml:"log_retention_days"`
}

type WorkspaceConfig struct {
	Default string `yaml:"default"`
}

type ProjectConfig struct {
	CWD      string `yaml:"cwd"`
	Model    string `yaml:"model"`
	Sandbox  string `yaml:"sandbox"`
	Approval string `yaml:"approval"`
}

type RuntimePaths struct {
	StateDB string `yaml:"state_db"`
	LogDir  string `yaml:"log_dir"`
}

type ResolvedProject struct {
	Alias            string
	Command          string
	CWD              string
	Model            string
	Sandbox          string
	Approval         string
	ExtraArgs        []string
	LogRetentionDays int
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
	if err := yaml.Unmarshal(data, &cfg); err != nil {
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
			continue
		}
		if stat != nil {
			if err := stat(project.CWD); err != nil {
				diags = append(diags, Diagnostic{Level: LevelError, Code: "project." + alias + ".cwd", Message: fmt.Sprintf("project cwd is not accessible: %v", err)})
			} else {
				diags = append(diags, Diagnostic{Level: LevelOK, Code: "project." + alias + ".cwd", Message: "project cwd exists"})
			}
		}
	}
	if cfg.Codex.LogRetentionDays <= 0 {
		diags = append(diags, Diagnostic{Level: LevelError, Code: "codex.log_retention_days", Message: "log retention days must be positive"})
	} else {
		diags = append(diags, Diagnostic{Level: LevelOK, Code: "codex.log_retention_days", Message: "log retention is positive"})
	}
	return diags
}

func (cfg Config) ResolveProject(alias string) (ResolvedProject, error) {
	cfg.applyDefaults("")
	resolved := ResolvedProject{
		Alias:            alias,
		Command:          cfg.Codex.Command,
		CWD:              cfg.Workspace.Default,
		Model:            cfg.Codex.DefaultModel,
		Sandbox:          cfg.Codex.Sandbox,
		Approval:         cfg.Codex.Approval,
		ExtraArgs:        append([]string(nil), cfg.Codex.ExtraArgs...),
		LogRetentionDays: cfg.Codex.LogRetentionDays,
	}
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
	if project.Sandbox != "" {
		resolved.Sandbox = project.Sandbox
	}
	if project.Approval != "" {
		resolved.Approval = project.Approval
	}
	return resolved, nil
}

func (cfg *Config) applyDefaults(home string) {
	if cfg.Feishu.Connection == "" {
		cfg.Feishu.Connection = defaultConnection
	}
	if cfg.Codex.Command == "" {
		cfg.Codex.Command = defaultCommand
	}
	if cfg.Codex.Sandbox == "" {
		cfg.Codex.Sandbox = defaultSandbox
	}
	if cfg.Codex.Approval == "" {
		cfg.Codex.Approval = defaultApproval
	}
	if cfg.Codex.LogRetentionDays == 0 {
		cfg.Codex.LogRetentionDays = defaultLogRetentionDays
	}
	if cfg.Projects == nil {
		cfg.Projects = map[string]ProjectConfig{}
	}
	if home != "" {
		if cfg.Paths.StateDB == "" {
			cfg.Paths.StateDB = filepath.Join(home, ".codex-feishu-bridge", "state.db")
		}
		if cfg.Paths.LogDir == "" {
			cfg.Paths.LogDir = filepath.Join(home, ".codex-feishu-bridge", "logs")
		}
	}
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
