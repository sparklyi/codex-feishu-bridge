package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/sparklyi/codex-feishu-bridge/internal/codexrunner"
	"github.com/sparklyi/codex-feishu-bridge/internal/config"
	"github.com/sparklyi/codex-feishu-bridge/internal/logs"
	"github.com/sparklyi/codex-feishu-bridge/internal/notifier"
	"github.com/sparklyi/codex-feishu-bridge/internal/router"
	"github.com/sparklyi/codex-feishu-bridge/internal/store"
	"github.com/sparklyi/codex-feishu-bridge/internal/transport"
	"github.com/sparklyi/codex-feishu-bridge/internal/transport/feishu"
)

type ServeOptions struct {
	ConfigPath string
	Getenv     func(string) string
	Receiver   transport.Receiver
	Sender     transport.Sender
	Runner     router.Runner
	Now        func() time.Time
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
	if err := logs.Prune(cfg.Paths.LogDir, cfg.Codex.LogRetentionDays, now()); err != nil {
		return err
	}
	startPruneLoop(ctx, cfg.Paths.LogDir, cfg.Codex.LogRetentionDays, now)
	receiver := opts.Receiver
	sender := opts.Sender
	secret := getenv(cfg.Feishu.AppSecretEnv)
	if receiver == nil {
		if secret == "" {
			return errors.New("missing Feishu app secret")
		}
		source := feishu.NewSDKEventSource(cfg.Feishu.AppID, secret, "")
		receiver = feishu.Receiver{Source: source, Verify: feishu.VerifyOptions{AppID: cfg.Feishu.AppID}}
	}
	if sender == nil {
		api := feishu.NewSDKCardAPI(cfg.Feishu.AppID, secret)
		sender, err = feishu.NewSenderFromEnv(cfg.Feishu.AppID, cfg.Feishu.AppSecretEnv, getenv, api)
		if err != nil {
			return err
		}
	}
	run := opts.Runner
	if run == nil {
		run = &codexrunner.Runner{LogDir: cfg.Paths.LogDir, Now: now}
	}
	notify := notifier.New(sender)
	rt := router.New(router.RouterOptions{Config: cfg, Store: st, Runner: run, Notifier: notify, Now: now})
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

func startPruneLoop(ctx context.Context, dir string, retentionDays int, now func() time.Time) {
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = logs.Prune(dir, retentionDays, now())
			}
		}
	}()
}

const defaultConfig = `feishu:
  app_id: cli_xxx
  app_secret_env: FEISHU_APP_SECRET
  connection: websocket
security:
  allowed_open_ids:
    - ou_xxx
codex:
  command: codex
  default_model: ""
  sandbox: workspace-write
  approval: never
  extra_args: []
  log_retention_days: 14
workspace:
  default: /path/to/default/repo
projects:
  backend:
    cwd: /path/to/backend
    model: ""
    sandbox: workspace-write
    approval: never
`
