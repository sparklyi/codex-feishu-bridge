package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/sparklyi/codex-feishu-bridge/internal/appserver"
	"github.com/sparklyi/codex-feishu-bridge/internal/config"
	"github.com/sparklyi/codex-feishu-bridge/internal/contracts"
	"github.com/sparklyi/codex-feishu-bridge/internal/notifier"
	"github.com/sparklyi/codex-feishu-bridge/internal/router"
	"github.com/sparklyi/codex-feishu-bridge/internal/runtime"
	"github.com/sparklyi/codex-feishu-bridge/internal/store"
	"github.com/sparklyi/codex-feishu-bridge/internal/transport"
	"github.com/sparklyi/codex-feishu-bridge/internal/transport/feishu"
)

// AppServer combines the runtime contract with the lifecycle ownership held by
// the composition root.
type AppServer interface {
	runtime.AppServer
	io.Closer
}

type OpenAppServer func(context.Context, appserver.ProcessOptions) (AppServer, error)

const supervisedEnv = "CODEX_FEISHU_BRIDGE_SUPERVISED"

// ErrRestartRequested is returned when a supervised bridge receives its native
// restart command. The process entrypoint maps it to the supervisor's restart
// exit code after all owned resources have shut down.
var ErrRestartRequested = errors.New("bridge restart requested")

type ServeOptions struct {
	ConfigPath    string
	Getenv        func(string) string
	Receiver      transport.Receiver
	Sender        transport.Sender
	AppServer     AppServer
	OpenAppServer OpenAppServer
	Now           func() time.Time
}

func Serve(ctx context.Context, opts ServeOptions) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	cfgPath := opts.ConfigPath
	if cfgPath == "" {
		cfgPath = config.DefaultPath(getenv("HOME"))
	}
	cfg, err := config.Load(cfgPath, getenv)
	if err != nil {
		return err
	}
	for _, diagnostic := range cfg.Validate(getenv, func(path string) error {
		_, err := os.Stat(path)
		return err
	}) {
		if diagnostic.Level == config.LevelError {
			return fmt.Errorf("invalid config %s: %s", diagnostic.Code, diagnostic.Message)
		}
	}
	proxyURL, err := cfg.Feishu.Proxy()
	if err != nil {
		return fmt.Errorf("invalid config feishu.proxy_url: %w", err)
	}
	networkOptions := feishuNetworkOptions(cfg.Feishu.Network)
	st, err := store.Open(ctx, cfg.Paths.StateDB)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := st.Close(); closeErr != nil {
			slog.Warn("bridge state store close failed", "error", closeErr)
		}
	}()
	if err := st.RefreshUsers(ctx, cfg.Security.AllowedOpenIDs); err != nil {
		return err
	}
	if err := st.RecoverRunning(ctx, now()); err != nil {
		return err
	}

	receiver := opts.Receiver
	sender := opts.Sender
	secret := getenv(cfg.Feishu.AppSecretEnv)
	if receiver == nil {
		if secret == "" {
			return errors.New("missing Feishu app secret")
		}
		source := feishu.NewSDKEventSourceWithOptions(cfg.Feishu.AppID, secret, proxyURL, networkOptions)
		receiver = feishu.Receiver{
			Source: source,
			Verify: feishu.VerifyOptions{AppID: cfg.Feishu.AppID},
			OnHandleError: func(_ context.Context, event contracts.InboundEvent, err error) {
				slog.Error("Feishu event handling failed", "kind", event.Kind, "action_id", event.ActionID, "error", err)
			},
		}
	}
	if sender == nil {
		api := feishu.NewSDKCardAPIWithOptions(cfg.Feishu.AppID, secret, proxyURL, networkOptions)
		sender, err = feishu.NewSenderFromEnvWithOptions(cfg.Feishu.AppID, cfg.Feishu.AppSecretEnv, getenv, api, feishu.SenderOptions{
			MaxAttempts:    cfg.Feishu.Network.DeliveryMaxAttempts,
			AttemptTimeout: cfg.Feishu.Network.DeliveryAttemptTimeout(),
			RetryDelay:     cfg.Feishu.Network.DeliveryRetryDelay(),
		})
		if err != nil {
			return err
		}
	}

	api := opts.AppServer
	if api == nil {
		open := opts.OpenAppServer
		if open == nil {
			open = func(ctx context.Context, processOpts appserver.ProcessOptions) (AppServer, error) {
				return appserver.Open(ctx, processOpts)
			}
		}
		api, err = open(ctx, appserver.ProcessOptions{Command: cfg.AppServer.Command, Version: "dev", Timeout: cfg.StartupTimeout()})
		if err != nil {
			return err
		}
	}
	defer func() {
		if closeErr := api.Close(); closeErr != nil {
			slog.Warn("Codex app-server close failed", "error", closeErr)
		}
	}()

	notify := notifier.New(sender, notifier.Options{CardDisplayMode: cfg.Feishu.CardDisplayMode})
	controller := runtime.New(runtime.ControllerOptions{
		AppServer:                    api,
		Store:                        st,
		Notifier:                     notify,
		CardDisplayMode:              cfg.Feishu.CardDisplayMode,
		Now:                          now,
		ProgressUpdateInterval:       cfg.Runtime.StreamUpdateInterval(),
		ProgressUpdateAttemptTimeout: cfg.Runtime.StreamUpdateAttemptTimeout(),
		ProgressRetryDelay:           cfg.Runtime.StreamRetryDelay(),
		NotificationTimeout:          cfg.Runtime.NotificationTimeout(),
		AppServerTimeout:             cfg.Runtime.AppServerTimeout(),
		TerminalRetryAttempts:        cfg.Runtime.TerminalRetryAttempts,
		TerminalRetryDelay:           cfg.Runtime.TerminalRetryDelay(),
	})
	defer controller.Close()
	probeCtx, cancelProbe := context.WithTimeout(ctx, cfg.StartupTimeout())
	err = controller.Probe(probeCtx)
	cancelProbe()
	if err != nil {
		return err
	}

	var restart func()
	if getenv(supervisedEnv) == "1" {
		restart = func() { cancel(ErrRestartRequested) }
	}
	rt := router.New(router.RouterOptions{
		Config:     cfg,
		Store:      st,
		Controller: controller,
		Notifier:   notify,
		Now:        now,
		Restart:    restart,
	})
	receiveErr := receiver.Receive(ctx, rt.Handle)
	if errors.Is(context.Cause(ctx), ErrRestartRequested) {
		return ErrRestartRequested
	}
	return receiveErr
}

func InitConfig(path string, force bool) error {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		path = config.DefaultPath(home)
	}
	if !force {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("config already exists: %s", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(defaultConfig), 0o600)
}

func ListTasks(ctx context.Context, configPath string, getenv func(string) string, limit int) ([]store.Task, error) {
	st, err := openStoreFromConfig(ctx, configPath, getenv)
	if err != nil {
		return nil, err
	}
	tasks, listErr := st.ListTasks(ctx, limit)
	closeErr := st.Close()
	if listErr != nil {
		return nil, listErr
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close state store: %w", closeErr)
	}
	return tasks, nil
}

func ShowTask(ctx context.Context, configPath string, getenv func(string) string, taskID string) (store.Task, []store.Run, error) {
	st, err := openStoreFromConfig(ctx, configPath, getenv)
	if err != nil {
		return store.Task{}, nil, err
	}
	task, runs, getErr := st.GetTask(ctx, taskID)
	closeErr := st.Close()
	if getErr != nil {
		return store.Task{}, nil, getErr
	}
	if closeErr != nil {
		return store.Task{}, nil, fmt.Errorf("close state store: %w", closeErr)
	}
	return task, runs, nil
}

func openStoreFromConfig(ctx context.Context, configPath string, getenv func(string) string) (*store.Store, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if configPath == "" {
		configPath = config.DefaultPath(getenv("HOME"))
	}
	cfg, err := config.Load(configPath, getenv)
	if err != nil {
		return nil, err
	}
	return store.Open(ctx, cfg.Paths.StateDB)
}

func feishuNetworkOptions(cfg config.FeishuNetworkConfig) feishu.NetworkOptions {
	return feishu.NetworkOptions{
		HTTPTimeout:                cfg.HTTPTimeout(),
		BootstrapTimeout:           cfg.BootstrapTimeout(),
		WebSocketFallbackHeartbeat: cfg.WebSocketFallbackHeartbeat(),
		WebSocketMaxHeartbeat:      cfg.WebSocketMaxHeartbeat(),
		ReconnectDelay:             cfg.ReconnectDelay(),
		WriteTimeout:               cfg.WriteTimeout(),
		FragmentTTL:                cfg.FragmentTTL(),
		SourceCloseTimeout:         cfg.SourceCloseTimeout(),
		MaxIdleConnections:         cfg.MaxIdleConnections,
		MaxIdleConnectionsPerHost:  cfg.MaxIdleConnectionsPerHost,
		IdleConnectionTimeout:      cfg.IdleConnectionTimeout(),
		DialKeepAlive:              cfg.DialKeepAlive(),
		EventQueueCapacity:         cfg.EventQueueCapacity,
		CardActionQueueCapacity:    cfg.CardActionQueueCapacity,
		FailureQueueCapacity:       cfg.FailureQueueCapacity,
	}
}

const defaultConfig = `feishu:
  app_id: cli_xxx
  app_secret_env: FEISHU_APP_SECRET
  # preview shows throttled processing detail; concise hides it.
  card_display_mode: preview
  # proxy_url: http://127.0.0.1:7890
  network:
    http_timeout_seconds: 30
    bootstrap_timeout_seconds: 8
    websocket_fallback_heartbeat_seconds: 30
    websocket_max_heartbeat_seconds: 30
    reconnect_delay_milliseconds: 1000
    write_timeout_seconds: 5
    fragment_ttl_seconds: 5
    source_close_timeout_seconds: 5
    max_idle_connections: 32
    max_idle_connections_per_host: 8
    idle_connection_timeout_seconds: 30
    dial_keep_alive_seconds: 20
    delivery_attempt_timeout_seconds: 5
    delivery_max_attempts: 3
    delivery_retry_delay_milliseconds: 100
    event_queue_capacity: 64
    card_action_queue_capacity: 64
    failure_queue_capacity: 1
security:
  allowed_open_ids:
    - ou_xxx
app_server:
  command: codex
  default_model: ""
  startup_timeout_seconds: 15
runtime:
  stream_update_interval_milliseconds: 200
  stream_update_attempt_timeout_milliseconds: 1500
  stream_retry_delay_milliseconds: 800
  notification_timeout_seconds: 20
  app_server_timeout_seconds: 30
  terminal_retry_attempts: 3
  terminal_retry_delay_milliseconds: 1000
  route_insert_attempts: 2
  thread_selection_limit: 8
  thread_lookup_limit: 32
workspace:
  default: /path/to/default/repo
`
