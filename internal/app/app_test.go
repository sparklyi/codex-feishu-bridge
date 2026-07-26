package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sparklyi/codex-feishu-bridge/internal/appserver"
	"github.com/sparklyi/codex-feishu-bridge/internal/contracts"
	"github.com/sparklyi/codex-feishu-bridge/internal/store"
	"github.com/sparklyi/codex-feishu-bridge/internal/transport"
)

func TestServeRequiresDesktopThreadProbeBeforeReceivingEvents(t *testing.T) {
	dir := t.TempDir()
	workspace := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := writeAppConfig(t, dir, workspace)
	receiver := &fakeReceiver{}
	api := &fakeAppServer{threads: []appserver.Thread{{ID: "desktop", CWD: workspace}}}
	if err := Serve(context.Background(), ServeOptions{ConfigPath: configPath, Getenv: appEnv(dir), Receiver: receiver, Sender: &fakeSender{}, AppServer: api}); err != nil {
		t.Fatal(err)
	}
	if api.listCalls == 0 || receiver.calls != 1 || !api.closed {
		t.Fatalf("serve did not probe/close app server: list=%d receiver=%d closed=%v", api.listCalls, receiver.calls, api.closed)
	}
}

func TestServeFailsBeforeReceiverWhenThreadDiscoveryFails(t *testing.T) {
	dir := t.TempDir()
	workspace := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	receiver := &fakeReceiver{}
	err := Serve(context.Background(), ServeOptions{ConfigPath: writeAppConfig(t, dir, workspace), Getenv: appEnv(dir), Receiver: receiver, Sender: &fakeSender{}, AppServer: &fakeAppServer{listErr: errors.New("desktop unavailable")}})
	if err == nil || !strings.Contains(err.Error(), "discover desktop Codex threads") || receiver.calls != 0 {
		t.Fatalf("expected startup probe failure, err=%v receiver=%d", err, receiver.calls)
	}
}

func TestServeRecoversStaleRunAndInitConfigUsesAppServer(t *testing.T) {
	dir := t.TempDir()
	workspace := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := writeAppConfig(t, dir, workspace)
	st, err := store.Open(context.Background(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AdmitNewTask(context.Background(), "stale", "message", store.CreateTaskInput{TaskID: "stale-task", RunID: "stale-run", CWD: workspace, CreatedBy: "ou_owner", ChatID: "chat", Prompt: "stale", Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := Serve(context.Background(), ServeOptions{ConfigPath: configPath, Getenv: appEnv(dir), Receiver: &fakeReceiver{}, Sender: &fakeSender{}, AppServer: &fakeAppServer{}}); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(context.Background(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	task, _, err := st.GetTask(context.Background(), "stale-task")
	if err != nil || task.Status != "failed" {
		t.Fatalf("stale task not recovered: %+v err=%v", task, err)
	}
	generated := filepath.Join(dir, "generated.yaml")
	if err := InitConfig(generated, false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(generated)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "app_server:") || !strings.Contains(string(data), "startup_timeout_seconds: 15") || !strings.Contains(string(data), "card_display_mode: concise") || strings.Contains(string(data), "approval:") || strings.Contains(string(data), "sandbox:") || strings.Contains(string(data), "bot_open_id:") || strings.Contains(string(data), "connection:") || strings.Contains(string(data), "projects:") {
		t.Fatalf("unexpected generated config:\n%s", data)
	}
}

func TestServeClosesDependenciesWhenContextIsCanceled(t *testing.T) {
	dir := t.TempDir()
	workspace := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	receiver := &blockingReceiver{started: make(chan struct{})}
	api := &fakeAppServer{threads: []appserver.Thread{{ID: "desktop", CWD: workspace}}}
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, ServeOptions{
			ConfigPath: writeAppConfig(t, dir, workspace),
			Getenv:     appEnv(dir),
			Receiver:   receiver,
			Sender:     &fakeSender{},
			AppServer:  api,
		})
	}()
	select {
	case <-receiver.started:
	case <-time.After(time.Second):
		t.Fatal("serve did not reach the receiver")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("serve error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("serve did not return after cancellation")
	}
	if !api.closed {
		t.Fatal("app server was not closed after cancellation")
	}
}

func writeAppConfig(t *testing.T, dir, workspace string) string {
	t.Helper()
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
  state_db: "` + filepath.Join(dir, "state.db") + `"
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func appEnv(dir string) func(string) string {
	return func(key string) string {
		switch key {
		case "HOME":
			return dir
		case "FEISHU_APP_SECRET":
			return "secret"
		}
		return ""
	}
}

type fakeReceiver struct{ calls int }

func (f *fakeReceiver) Receive(_ context.Context, _ func(context.Context, contracts.InboundEvent) error) error {
	f.calls++
	return nil
}

type blockingReceiver struct {
	started chan struct{}
}

func (r *blockingReceiver) Receive(ctx context.Context, _ func(context.Context, contracts.InboundEvent) error) error {
	close(r.started)
	<-ctx.Done()
	return ctx.Err()
}

type fakeSender struct{}

func (*fakeSender) Send(context.Context, contracts.OutboundMessage) (contracts.SentMessage, error) {
	return contracts.SentMessage{MessageID: "card"}, nil
}

type fakeAppServer struct {
	threads   []appserver.Thread
	listErr   error
	listCalls int
	closed    bool
	events    chan appserver.Event
	requests  chan appserver.ServerRequest
}

func (f *fakeAppServer) ListThreads(context.Context, int) ([]appserver.Thread, error) {
	f.listCalls++
	return f.threads, f.listErr
}
func (*fakeAppServer) StartThread(context.Context, appserver.ThreadStartInput) (appserver.Thread, error) {
	return appserver.Thread{}, errors.New("not used")
}
func (*fakeAppServer) ResumeThread(context.Context, appserver.ThreadResumeInput) (appserver.Thread, error) {
	return appserver.Thread{}, errors.New("not used")
}
func (*fakeAppServer) StartTurn(context.Context, appserver.TurnStartInput) (appserver.Turn, error) {
	return appserver.Turn{}, errors.New("not used")
}
func (*fakeAppServer) SteerTurn(context.Context, appserver.TurnSteerInput) (string, error) {
	return "", errors.New("not used")
}
func (*fakeAppServer) Interrupt(context.Context, string, string) error     { return errors.New("not used") }
func (*fakeAppServer) Respond(context.Context, json.RawMessage, any) error { return nil }
func (f *fakeAppServer) Events() <-chan appserver.Event {
	if f.events == nil {
		f.events = make(chan appserver.Event)
	}
	return f.events
}
func (f *fakeAppServer) Requests() <-chan appserver.ServerRequest {
	if f.requests == nil {
		f.requests = make(chan appserver.ServerRequest)
	}
	return f.requests
}
func (f *fakeAppServer) Close() error { f.closed = true; return nil }

var _ transport.Receiver = (*fakeReceiver)(nil)
