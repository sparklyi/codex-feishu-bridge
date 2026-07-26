package app

import (
	"context"
	"errors"
	"fmt"
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

type OpenAppServer func(context.Context, appserver.ProcessOptions) (appserver.API, error)

type ServeOptions struct {
	ConfigPath    string
	Getenv        func(string) string
	Receiver      transport.Receiver
	Sender        transport.Sender
	AppServer     appserver.API
	OpenAppServer OpenAppServer
	Now           func() time.Time
}

func Serve(ctx context.Context, opts ServeOptions) error {
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
	st, err := store.Open(ctx, cfg.Paths.StateDB)
	if err != nil {
		return err
	}
	defer st.Close()
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
		source := feishu.NewSDKEventSource(cfg.Feishu.AppID, secret, proxyURL)
		receiver = feishu.Receiver{
			Source: source,
			Verify: feishu.VerifyOptions{AppID: cfg.Feishu.AppID},
			OnHandleError: func(_ context.Context, event contracts.InboundEvent, err error) {
				slog.Error("Feishu event handling failed", "kind", event.Kind, "action_id", event.ActionID, "error", err)
			},
		}
	}
	if sender == nil {
		api := feishu.NewSDKCardAPI(cfg.Feishu.AppID, secret, proxyURL)
		sender, err = feishu.NewSenderFromEnv(cfg.Feishu.AppID, cfg.Feishu.AppSecretEnv, getenv, api)
		if err != nil {
			return err
		}
	}

	api := opts.AppServer
	if api == nil {
		open := opts.OpenAppServer
		if open == nil {
			open = func(ctx context.Context, processOpts appserver.ProcessOptions) (appserver.API, error) {
				return appserver.Open(ctx, processOpts)
			}
		}
		api, err = open(ctx, appserver.ProcessOptions{Command: cfg.AppServer.Command, Version: "dev", Timeout: cfg.StartupTimeout()})
		if err != nil {
			return err
		}
	}
	defer api.Close()

	notify := notifier.New(sender)
	controller := runtime.New(runtime.ControllerOptions{
		AppServer: api,
		Store:     st,
		Notifier:  notify,
		Now:       now,
	})
	defer controller.Close()
	probeCtx, cancelProbe := context.WithTimeout(ctx, cfg.StartupTimeout())
	err = controller.Probe(probeCtx)
	cancelProbe()
	if err != nil {
		return err
	}

	rt := router.New(router.RouterOptions{Config: cfg, Store: st, Controller: controller, Notifier: notify, Now: now})
	return receiver.Receive(ctx, rt.Handle)
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
	defer st.Close()
	return st.ListTasks(ctx, limit)
}

func ShowTask(ctx context.Context, configPath string, getenv func(string) string, taskID string) (store.Task, []store.Run, error) {
	st, err := openStoreFromConfig(ctx, configPath, getenv)
	if err != nil {
		return store.Task{}, nil, err
	}
	defer st.Close()
	return st.GetTask(ctx, taskID)
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

const defaultConfig = `feishu:
  app_id: cli_xxx
  app_secret_env: FEISHU_APP_SECRET
  # proxy_url: http://127.0.0.1:7890
security:
  allowed_open_ids:
    - ou_xxx
app_server:
  command: codex
  default_model: ""
  startup_timeout_seconds: 15
workspace:
  default: /path/to/default/repo
`
