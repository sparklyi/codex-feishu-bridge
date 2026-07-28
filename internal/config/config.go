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
	defaultCommand                                = "codex"
	defaultStartupTimeoutSeconds                  = 15
	defaultCardDisplayMode                        = "preview"
	defaultStreamUpdateIntervalMilliseconds       = 200
	defaultStreamUpdateAttemptTimeoutMilliseconds = 1500
	defaultStreamRetryDelayMilliseconds           = 800
	defaultNotificationTimeoutSeconds             = 20
	defaultAppServerTimeoutSeconds                = 30
	defaultTerminalRetryAttempts                  = 3
	defaultTerminalRetryDelayMilliseconds         = 1000
	defaultRouteInsertAttempts                    = 2
	defaultThreadSelectionLimit                   = 8
	defaultThreadLookupLimit                      = 32
	defaultFeishuHTTPTimeoutSeconds               = 30
	defaultFeishuBootstrapTimeoutSeconds          = 8
	defaultFeishuFallbackHeartbeatSeconds         = 30
	defaultFeishuMaxHeartbeatSeconds              = 30
	defaultFeishuReconnectDelayMilliseconds       = 1000
	defaultFeishuWriteTimeoutSeconds              = 5
	defaultFeishuFragmentTTLSeconds               = 5
	defaultFeishuSourceCloseTimeoutSeconds        = 5
	defaultFeishuMaxIdleConnections               = 32
	defaultFeishuMaxIdleConnectionsPerHost        = 8
	defaultFeishuIdleConnectionTimeoutSeconds     = 30
	defaultFeishuDialKeepAliveSeconds             = 20
	defaultFeishuDeliveryAttemptTimeoutSeconds    = 5
	defaultFeishuDeliveryMaxAttempts              = 3
	defaultFeishuDeliveryRetryDelayMilliseconds   = 100
	defaultFeishuEventQueueCapacity               = 64
	defaultFeishuCardActionQueueCapacity          = 64
	defaultFeishuFailureQueueCapacity             = 1
)

type Config struct {
	Feishu    FeishuConfig             `yaml:"feishu"`
	Security  SecurityConfig           `yaml:"security"`
	AppServer AppServerConfig          `yaml:"app_server"`
	Workspace WorkspaceConfig          `yaml:"workspace"`
	Projects  map[string]ProjectConfig `yaml:"projects"`
	Paths     RuntimePaths             `yaml:"paths"`
	Runtime   RuntimeConfig            `yaml:"runtime"`
}

type FeishuConfig struct {
	AppID           string              `yaml:"app_id"`
	AppSecretEnv    string              `yaml:"app_secret_env"`
	ProxyURL        string              `yaml:"proxy_url"`
	CardDisplayMode string              `yaml:"card_display_mode"`
	Network         FeishuNetworkConfig `yaml:"network"`
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
	ExperimentalAPI       bool   `yaml:"experimental_api"`
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

// RuntimeConfig controls task execution and task-card update behavior.
type RuntimeConfig struct {
	StreamUpdateIntervalMilliseconds       int `yaml:"stream_update_interval_milliseconds"`
	StreamUpdateAttemptTimeoutMilliseconds int `yaml:"stream_update_attempt_timeout_milliseconds"`
	StreamRetryDelayMilliseconds           int `yaml:"stream_retry_delay_milliseconds"`
	NotificationTimeoutSeconds             int `yaml:"notification_timeout_seconds"`
	AppServerTimeoutSeconds                int `yaml:"app_server_timeout_seconds"`
	TerminalRetryAttempts                  int `yaml:"terminal_retry_attempts"`
	TerminalRetryDelayMilliseconds         int `yaml:"terminal_retry_delay_milliseconds"`
	RouteInsertAttempts                    int `yaml:"route_insert_attempts"`
	ThreadSelectionLimit                   int `yaml:"thread_selection_limit"`
	ThreadLookupLimit                      int `yaml:"thread_lookup_limit"`
}

// FeishuNetworkConfig controls Feishu connection and card-delivery behavior.
type FeishuNetworkConfig struct {
	HTTPTimeoutSeconds                int `yaml:"http_timeout_seconds"`
	BootstrapTimeoutSeconds           int `yaml:"bootstrap_timeout_seconds"`
	WebSocketFallbackHeartbeatSeconds int `yaml:"websocket_fallback_heartbeat_seconds"`
	WebSocketMaxHeartbeatSeconds      int `yaml:"websocket_max_heartbeat_seconds"`
	ReconnectDelayMilliseconds        int `yaml:"reconnect_delay_milliseconds"`
	WriteTimeoutSeconds               int `yaml:"write_timeout_seconds"`
	FragmentTTLSeconds                int `yaml:"fragment_ttl_seconds"`
	SourceCloseTimeoutSeconds         int `yaml:"source_close_timeout_seconds"`
	MaxIdleConnections                int `yaml:"max_idle_connections"`
	MaxIdleConnectionsPerHost         int `yaml:"max_idle_connections_per_host"`
	IdleConnectionTimeoutSeconds      int `yaml:"idle_connection_timeout_seconds"`
	DialKeepAliveSeconds              int `yaml:"dial_keep_alive_seconds"`
	DeliveryAttemptTimeoutSeconds     int `yaml:"delivery_attempt_timeout_seconds"`
	DeliveryMaxAttempts               int `yaml:"delivery_max_attempts"`
	DeliveryRetryDelayMilliseconds    int `yaml:"delivery_retry_delay_milliseconds"`
	EventQueueCapacity                int `yaml:"event_queue_capacity"`
	CardActionQueueCapacity           int `yaml:"card_action_queue_capacity"`
	FailureQueueCapacity              int `yaml:"failure_queue_capacity"`
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
	for _, setting := range []struct {
		code  string
		value int
	}{
		{"runtime.stream_update_interval_milliseconds", cfg.Runtime.StreamUpdateIntervalMilliseconds},
		{"runtime.stream_update_attempt_timeout_milliseconds", cfg.Runtime.StreamUpdateAttemptTimeoutMilliseconds},
		{"runtime.stream_retry_delay_milliseconds", cfg.Runtime.StreamRetryDelayMilliseconds},
		{"runtime.notification_timeout_seconds", cfg.Runtime.NotificationTimeoutSeconds},
		{"runtime.app_server_timeout_seconds", cfg.Runtime.AppServerTimeoutSeconds},
		{"runtime.terminal_retry_attempts", cfg.Runtime.TerminalRetryAttempts},
		{"runtime.terminal_retry_delay_milliseconds", cfg.Runtime.TerminalRetryDelayMilliseconds},
		{"runtime.route_insert_attempts", cfg.Runtime.RouteInsertAttempts},
		{"runtime.thread_selection_limit", cfg.Runtime.ThreadSelectionLimit},
		{"runtime.thread_lookup_limit", cfg.Runtime.ThreadLookupLimit},
		{"feishu.network.http_timeout_seconds", cfg.Feishu.Network.HTTPTimeoutSeconds},
		{"feishu.network.bootstrap_timeout_seconds", cfg.Feishu.Network.BootstrapTimeoutSeconds},
		{"feishu.network.websocket_fallback_heartbeat_seconds", cfg.Feishu.Network.WebSocketFallbackHeartbeatSeconds},
		{"feishu.network.websocket_max_heartbeat_seconds", cfg.Feishu.Network.WebSocketMaxHeartbeatSeconds},
		{"feishu.network.reconnect_delay_milliseconds", cfg.Feishu.Network.ReconnectDelayMilliseconds},
		{"feishu.network.write_timeout_seconds", cfg.Feishu.Network.WriteTimeoutSeconds},
		{"feishu.network.fragment_ttl_seconds", cfg.Feishu.Network.FragmentTTLSeconds},
		{"feishu.network.source_close_timeout_seconds", cfg.Feishu.Network.SourceCloseTimeoutSeconds},
		{"feishu.network.max_idle_connections", cfg.Feishu.Network.MaxIdleConnections},
		{"feishu.network.max_idle_connections_per_host", cfg.Feishu.Network.MaxIdleConnectionsPerHost},
		{"feishu.network.idle_connection_timeout_seconds", cfg.Feishu.Network.IdleConnectionTimeoutSeconds},
		{"feishu.network.dial_keep_alive_seconds", cfg.Feishu.Network.DialKeepAliveSeconds},
		{"feishu.network.delivery_attempt_timeout_seconds", cfg.Feishu.Network.DeliveryAttemptTimeoutSeconds},
		{"feishu.network.delivery_max_attempts", cfg.Feishu.Network.DeliveryMaxAttempts},
		{"feishu.network.delivery_retry_delay_milliseconds", cfg.Feishu.Network.DeliveryRetryDelayMilliseconds},
		{"feishu.network.event_queue_capacity", cfg.Feishu.Network.EventQueueCapacity},
		{"feishu.network.card_action_queue_capacity", cfg.Feishu.Network.CardActionQueueCapacity},
		{"feishu.network.failure_queue_capacity", cfg.Feishu.Network.FailureQueueCapacity},
	} {
		if setting.value <= 0 {
			diags = append(diags, Diagnostic{Level: LevelError, Code: setting.code, Message: "must be positive"})
		}
	}
	if cfg.Feishu.Network.WebSocketFallbackHeartbeatSeconds > cfg.Feishu.Network.WebSocketMaxHeartbeatSeconds {
		diags = append(diags, Diagnostic{Level: LevelError, Code: "feishu.network.websocket_max_heartbeat_seconds", Message: "must not be smaller than websocket_fallback_heartbeat_seconds"})
	}
	if cfg.Feishu.Network.MaxIdleConnectionsPerHost > cfg.Feishu.Network.MaxIdleConnections {
		diags = append(diags, Diagnostic{Level: LevelError, Code: "feishu.network.max_idle_connections_per_host", Message: "must not exceed max_idle_connections"})
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

func (cfg RuntimeConfig) StreamUpdateInterval() time.Duration {
	cfg.applyDefaults()
	return milliseconds(cfg.StreamUpdateIntervalMilliseconds)
}

func (cfg RuntimeConfig) StreamUpdateAttemptTimeout() time.Duration {
	cfg.applyDefaults()
	return milliseconds(cfg.StreamUpdateAttemptTimeoutMilliseconds)
}

func (cfg RuntimeConfig) StreamRetryDelay() time.Duration {
	cfg.applyDefaults()
	return milliseconds(cfg.StreamRetryDelayMilliseconds)
}

func (cfg RuntimeConfig) NotificationTimeout() time.Duration {
	cfg.applyDefaults()
	return seconds(cfg.NotificationTimeoutSeconds)
}

func (cfg RuntimeConfig) AppServerTimeout() time.Duration {
	cfg.applyDefaults()
	return seconds(cfg.AppServerTimeoutSeconds)
}

func (cfg RuntimeConfig) TerminalRetryDelay() time.Duration {
	cfg.applyDefaults()
	return milliseconds(cfg.TerminalRetryDelayMilliseconds)
}

func (cfg RuntimeConfig) ThreadSelectionLimitValue() int {
	cfg.applyDefaults()
	return cfg.ThreadSelectionLimit
}

func (cfg RuntimeConfig) RouteInsertAttemptsValue() int {
	cfg.applyDefaults()
	return cfg.RouteInsertAttempts
}

func (cfg RuntimeConfig) ThreadLookupLimitValue() int {
	cfg.applyDefaults()
	return cfg.ThreadLookupLimit
}

func (cfg FeishuNetworkConfig) HTTPTimeout() time.Duration {
	cfg.applyDefaults()
	return seconds(cfg.HTTPTimeoutSeconds)
}

func (cfg FeishuNetworkConfig) BootstrapTimeout() time.Duration {
	cfg.applyDefaults()
	return seconds(cfg.BootstrapTimeoutSeconds)
}

func (cfg FeishuNetworkConfig) WebSocketFallbackHeartbeat() time.Duration {
	cfg.applyDefaults()
	return seconds(cfg.WebSocketFallbackHeartbeatSeconds)
}

func (cfg FeishuNetworkConfig) WebSocketMaxHeartbeat() time.Duration {
	cfg.applyDefaults()
	return seconds(cfg.WebSocketMaxHeartbeatSeconds)
}

func (cfg FeishuNetworkConfig) ReconnectDelay() time.Duration {
	cfg.applyDefaults()
	return milliseconds(cfg.ReconnectDelayMilliseconds)
}

func (cfg FeishuNetworkConfig) WriteTimeout() time.Duration {
	cfg.applyDefaults()
	return seconds(cfg.WriteTimeoutSeconds)
}

func (cfg FeishuNetworkConfig) FragmentTTL() time.Duration {
	cfg.applyDefaults()
	return seconds(cfg.FragmentTTLSeconds)
}

func (cfg FeishuNetworkConfig) SourceCloseTimeout() time.Duration {
	cfg.applyDefaults()
	return seconds(cfg.SourceCloseTimeoutSeconds)
}

func (cfg FeishuNetworkConfig) IdleConnectionTimeout() time.Duration {
	cfg.applyDefaults()
	return seconds(cfg.IdleConnectionTimeoutSeconds)
}

func (cfg FeishuNetworkConfig) DialKeepAlive() time.Duration {
	cfg.applyDefaults()
	return seconds(cfg.DialKeepAliveSeconds)
}

func (cfg FeishuNetworkConfig) DeliveryAttemptTimeout() time.Duration {
	cfg.applyDefaults()
	return seconds(cfg.DeliveryAttemptTimeoutSeconds)
}

func (cfg FeishuNetworkConfig) DeliveryRetryDelay() time.Duration {
	cfg.applyDefaults()
	return milliseconds(cfg.DeliveryRetryDelayMilliseconds)
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
	cfg.Feishu.Network.applyDefaults()
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
	cfg.Runtime.applyDefaults()
}

func (cfg *RuntimeConfig) applyDefaults() {
	if cfg.StreamUpdateIntervalMilliseconds == 0 {
		cfg.StreamUpdateIntervalMilliseconds = defaultStreamUpdateIntervalMilliseconds
	}
	if cfg.StreamUpdateAttemptTimeoutMilliseconds == 0 {
		cfg.StreamUpdateAttemptTimeoutMilliseconds = defaultStreamUpdateAttemptTimeoutMilliseconds
	}
	if cfg.StreamRetryDelayMilliseconds == 0 {
		cfg.StreamRetryDelayMilliseconds = defaultStreamRetryDelayMilliseconds
	}
	if cfg.NotificationTimeoutSeconds == 0 {
		cfg.NotificationTimeoutSeconds = defaultNotificationTimeoutSeconds
	}
	if cfg.AppServerTimeoutSeconds == 0 {
		cfg.AppServerTimeoutSeconds = defaultAppServerTimeoutSeconds
	}
	if cfg.TerminalRetryAttempts == 0 {
		cfg.TerminalRetryAttempts = defaultTerminalRetryAttempts
	}
	if cfg.TerminalRetryDelayMilliseconds == 0 {
		cfg.TerminalRetryDelayMilliseconds = defaultTerminalRetryDelayMilliseconds
	}
	if cfg.RouteInsertAttempts == 0 {
		cfg.RouteInsertAttempts = defaultRouteInsertAttempts
	}
	if cfg.ThreadSelectionLimit == 0 {
		cfg.ThreadSelectionLimit = defaultThreadSelectionLimit
	}
	if cfg.ThreadLookupLimit == 0 {
		cfg.ThreadLookupLimit = defaultThreadLookupLimit
	}
}

func (cfg *FeishuNetworkConfig) applyDefaults() {
	if cfg.HTTPTimeoutSeconds == 0 {
		cfg.HTTPTimeoutSeconds = defaultFeishuHTTPTimeoutSeconds
	}
	if cfg.BootstrapTimeoutSeconds == 0 {
		cfg.BootstrapTimeoutSeconds = defaultFeishuBootstrapTimeoutSeconds
	}
	if cfg.WebSocketFallbackHeartbeatSeconds == 0 {
		cfg.WebSocketFallbackHeartbeatSeconds = defaultFeishuFallbackHeartbeatSeconds
	}
	if cfg.WebSocketMaxHeartbeatSeconds == 0 {
		cfg.WebSocketMaxHeartbeatSeconds = defaultFeishuMaxHeartbeatSeconds
	}
	if cfg.ReconnectDelayMilliseconds == 0 {
		cfg.ReconnectDelayMilliseconds = defaultFeishuReconnectDelayMilliseconds
	}
	if cfg.WriteTimeoutSeconds == 0 {
		cfg.WriteTimeoutSeconds = defaultFeishuWriteTimeoutSeconds
	}
	if cfg.FragmentTTLSeconds == 0 {
		cfg.FragmentTTLSeconds = defaultFeishuFragmentTTLSeconds
	}
	if cfg.SourceCloseTimeoutSeconds == 0 {
		cfg.SourceCloseTimeoutSeconds = defaultFeishuSourceCloseTimeoutSeconds
	}
	if cfg.MaxIdleConnections == 0 {
		cfg.MaxIdleConnections = defaultFeishuMaxIdleConnections
	}
	if cfg.MaxIdleConnectionsPerHost == 0 {
		cfg.MaxIdleConnectionsPerHost = defaultFeishuMaxIdleConnectionsPerHost
	}
	if cfg.IdleConnectionTimeoutSeconds == 0 {
		cfg.IdleConnectionTimeoutSeconds = defaultFeishuIdleConnectionTimeoutSeconds
	}
	if cfg.DialKeepAliveSeconds == 0 {
		cfg.DialKeepAliveSeconds = defaultFeishuDialKeepAliveSeconds
	}
	if cfg.DeliveryAttemptTimeoutSeconds == 0 {
		cfg.DeliveryAttemptTimeoutSeconds = defaultFeishuDeliveryAttemptTimeoutSeconds
	}
	if cfg.DeliveryMaxAttempts == 0 {
		cfg.DeliveryMaxAttempts = defaultFeishuDeliveryMaxAttempts
	}
	if cfg.DeliveryRetryDelayMilliseconds == 0 {
		cfg.DeliveryRetryDelayMilliseconds = defaultFeishuDeliveryRetryDelayMilliseconds
	}
	if cfg.EventQueueCapacity == 0 {
		cfg.EventQueueCapacity = defaultFeishuEventQueueCapacity
	}
	if cfg.CardActionQueueCapacity == 0 {
		cfg.CardActionQueueCapacity = defaultFeishuCardActionQueueCapacity
	}
	if cfg.FailureQueueCapacity == 0 {
		cfg.FailureQueueCapacity = defaultFeishuFailureQueueCapacity
	}
}

func milliseconds(value int) time.Duration {
	return time.Duration(value) * time.Millisecond
}

func seconds(value int) time.Duration {
	return time.Duration(value) * time.Second
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
