# Feishu Chat UX Improvements Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the first version of the improved Feishu chat UX: private plain-message tasks, group @bot + @project tasks, compact status cards, running-task conflict feedback, project selection cards, and shortcut actions.

**Architecture:** Keep the existing task/run/message-route model. Add a focused intent parser in front of the router, extend normalized Feishu events with mention/action metadata, add minimal store support for pending project selection and active-task lookup, then enhance notifier/card rendering without putting Feishu JSON details in router.

**Tech Stack:** Go 1.26, SQLite via `modernc.org/sqlite`, Feishu interactive cards through `larksuite/oapi-sdk-go`, existing `go test ./...` and `go vet ./...`.

---

## File Structure

- Create `internal/intent/intent.go`: parser-facing types and action constants.
- Create `internal/intent/parser.go`: parse normalized events into start, ignored, migration-hint, and project-selection intents.
- Create `internal/intent/parser_test.go`: parser unit tests independent of Feishu SDK and SQLite.
- Modify `internal/contracts/contracts.go`: add normalized mention/action value fields and richer outbound action/card metadata.
- Modify `internal/config/config.go`: add `feishu.bot_open_id` and helpers for configured project aliases.
- Modify `internal/config/config_test.go`: validate YAML/default behavior and project alias helper.
- Modify `config.example.yaml`, `README.md`, `README.zh-CN.md`, and setup docs for the new UX.
- Modify `internal/transport/feishu/normalizer.go`: parse Feishu mention metadata and card callback values into contracts.
- Modify `internal/transport/feishu/normalizer_test.go`: cover bot mentions, action values, and existing reply behavior.
- Modify `internal/store/schema.go`: add migration v2 for `pending_intents`; preserve v1 state.
- Modify `internal/store/store.go`: add active task lookup, pending intent admission/consume/expire, and task timestamp fields for elapsed status.
- Modify `internal/store/store_test.go`: cover new store APIs and migration behavior.
- Modify `internal/router/router.go`: replace command-only new-task parsing with intent-driven flow; keep continuation route lookup explicit.
- Modify `internal/router/command.go` or retire it after migration; do not keep `/codex` as an execution path.
- Modify `internal/router/router_test.go`: cover private plain text, group mention flows, running conflicts, project selection, and shortcuts.
- Modify `internal/notifier/notifier.go`: add compact card content methods and shortcut/project-selection card inputs.
- Modify `internal/notifier/notifier_test.go`: assert redaction, card metadata, and shortcut payloads.
- Modify `internal/transport/feishu/sender.go`: render compact Feishu cards with header templates, metadata fields, multiple buttons, and callback values.
- Modify `internal/transport/feishu/sender_test.go`: validate card JSON shape and values.
- Modify `internal/app/integration_test.go` and `internal/app/app_test.go`: update end-to-end flows away from `/codex`.

## Chunk 1: Intent Parsing And Configuration

### Task 1: Add bot identity and project alias helpers

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `config.example.yaml`

- [x] **Step 1: Write failing config tests**

Add tests in `internal/config/config_test.go`:

```go
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
```

- [x] **Step 2: Run config tests and verify failure**

Run: `go test ./internal/config -run 'TestLoadFeishuBotOpenID|TestProjectAliasesSorted' -count=1`

Expected: FAIL because `FeishuConfig.BotOpenID` and `ProjectAliases` do not exist.

- [x] **Step 3: Implement config additions**

Add to `internal/config/config.go`:

```go
type FeishuConfig struct {
	AppID        string `yaml:"app_id"`
	AppSecretEnv string `yaml:"app_secret_env"`
	Connection   string `yaml:"connection"`
	BotOpenID    string `yaml:"bot_open_id"`
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
```

Import `sort`. Update `config.example.yaml` with:

```yaml
feishu:
  app_id: cli_xxx
  app_secret_env: FEISHU_APP_SECRET
  bot_open_id: ou_bot_xxx
  connection: websocket
```

- [x] **Step 4: Run config tests and verify pass**

Run: `go test ./internal/config -count=1`

Expected: PASS.

- [x] **Step 5: Commit config support**

```bash
git add internal/config/config.go internal/config/config_test.go config.example.yaml
git commit -m "feat(config): add Feishu bot identity"
```

### Task 2: Create the intent parser

**Files:**
- Create: `internal/intent/intent.go`
- Create: `internal/intent/parser.go`
- Create: `internal/intent/parser_test.go`

- [x] **Step 1: Write failing parser tests**

Create `internal/intent/parser_test.go` with tests for:

```go
func TestPrivatePlainTextStartsDefaultTask(t *testing.T) {
	got := ParseStart(ParseInput{
		Event: contracts.InboundEvent{ChatType: "private", Text: "fix tests"},
		ProjectAliases: []string{"backend"},
	})
	if got.Kind != KindStartTask || got.Prompt != "fix tests" || got.ProjectAlias != "" {
		t.Fatalf("unexpected intent: %+v", got)
	}
}

func TestPrivateProjectPrefix(t *testing.T) {
	got := ParseStart(ParseInput{
		Event: contracts.InboundEvent{ChatType: "private", Text: "@backend fix tests"},
		ProjectAliases: []string{"backend"},
	})
	if got.Kind != KindStartTask || got.ProjectAlias != "backend" || got.Prompt != "fix tests" {
		t.Fatalf("unexpected intent: %+v", got)
	}
}

func TestPrivateUnknownProject(t *testing.T) {
	got := ParseStart(ParseInput{
		Event: contracts.InboundEvent{ChatType: "private", Text: "@missing fix tests"},
		ProjectAliases: []string{"backend"},
	})
	if got.Kind != KindUnknownProject || got.ProjectAlias != "missing" {
		t.Fatalf("unexpected intent: %+v", got)
	}
}

func TestCodexCommandReturnsMigrationHint(t *testing.T) {
	got := ParseStart(ParseInput{
		Event: contracts.InboundEvent{ChatType: "private", Text: "/codex fix tests"},
		ProjectAliases: []string{"backend"},
	})
	if got.Kind != KindMigrationHint {
		t.Fatalf("unexpected intent: %+v", got)
	}
}

func TestGroupRequiresBotMentionAndProject(t *testing.T) {
	plain := ParseStart(ParseInput{
		Event: contracts.InboundEvent{ChatType: "group", Text: "@backend fix tests"},
		ProjectAliases: []string{"backend"},
	})
	if plain.Kind != KindIgnored {
		t.Fatalf("plain group message should be ignored: %+v", plain)
	}
	missingProject := ParseStart(ParseInput{
		Event: contracts.InboundEvent{ChatType: "group", Text: "fix tests", BotMentioned: true},
		ProjectAliases: []string{"backend"},
	})
	if missingProject.Kind != KindProjectSelection {
		t.Fatalf("group mention without project should select project: %+v", missingProject)
	}
	start := ParseStart(ParseInput{
		Event: contracts.InboundEvent{ChatType: "group", Text: "@backend fix tests", BotMentioned: true},
		ProjectAliases: []string{"backend"},
	})
	if start.Kind != KindStartTask || start.ProjectAlias != "backend" || start.Prompt != "fix tests" {
		t.Fatalf("unexpected start intent: %+v", start)
	}
}
```

- [x] **Step 2: Run parser tests and verify failure**

Run: `go test ./internal/intent -count=1`

Expected: FAIL because the package does not exist.

- [x] **Step 3: Implement parser types and minimal logic**

Create `internal/intent/intent.go`:

```go
package intent

import "github.com/sparklyi/codex-feishu-bridge/internal/contracts"

type Kind string

const (
	KindIgnored          Kind = "ignored"
	KindStartTask        Kind = "start_task"
	KindProjectSelection Kind = "project_selection"
	KindUnknownProject   Kind = "unknown_project"
	KindMigrationHint    Kind = "migration_hint"
)

type ParseInput struct {
	Event          contracts.InboundEvent
	ProjectAliases []string
}

type Intent struct {
	Kind         Kind
	Prompt       string
	ProjectAlias string
}
```

Create `internal/intent/parser.go` with a small parser that:

- trims text;
- returns `KindMigrationHint` when text starts with `/codex`;
- ignores non-mentioned group root messages;
- parses a leading `@alias`;
- checks aliases with exact string match;
- requires project selection for mentioned group messages with no leading alias;
- returns start intent for private plain text.

- [x] **Step 4: Run parser tests and verify pass**

Run: `go test ./internal/intent -count=1`

Expected: PASS.

- [x] **Step 5: Commit intent parser**

```bash
git add internal/intent
git commit -m "feat(router): add Feishu intent parser"
```

## Chunk 2: Normalized Event Metadata

### Task 3: Add bot mention metadata to normalized events

**Files:**
- Modify: `internal/contracts/contracts.go`
- Modify: `internal/transport/feishu/normalizer.go`
- Modify: `internal/transport/feishu/normalizer_test.go`
- Modify: `internal/app/app.go`

- [x] **Step 1: Write failing normalizer tests**

Add tests in `internal/transport/feishu/normalizer_test.go`:

```go
func TestNormalizeMessageBotMention(t *testing.T) {
	raw := messageJSONWithMentions(t, map[string]any{"text": "@_user_1 @backend fix tests"}, []string{"ou_bot"})
	ev, err := NormalizeMessageJSON(raw, VerifyOptions{AppID: "cli_test", VerificationToken: "verify", BotOpenID: "ou_bot"})
	if err != nil {
		t.Fatal(err)
	}
	if !ev.BotMentioned {
		t.Fatalf("bot mention not detected: %+v", ev)
	}
	if ev.Text != "@backend fix tests" {
		t.Fatalf("bot mention should be stripped, text=%q", ev.Text)
	}
}

func TestNormalizeMessageNonBotMentionIsNotStripped(t *testing.T) {
	raw := messageJSONWithMentions(t, map[string]any{"text": "@someone @backend fix tests"}, []string{"ou_other"})
	ev, err := NormalizeMessageJSON(raw, VerifyOptions{AppID: "cli_test", VerificationToken: "verify", BotOpenID: "ou_bot"})
	if err != nil {
		t.Fatal(err)
	}
	if ev.BotMentioned || ev.Text != "@someone @backend fix tests" {
		t.Fatalf("unexpected mention handling: %+v", ev)
	}
}
```

- [x] **Step 2: Run normalizer tests and verify failure**

Run: `go test ./internal/transport/feishu -run 'TestNormalizeMessageBotMention|TestNormalizeMessageNonBotMentionIsNotStripped' -count=1`

Expected: FAIL because `BotMentioned`, `BotOpenID`, and mention parsing do not exist.

- [x] **Step 3: Implement normalized mention support**

Update `contracts.InboundEvent`:

```go
BotMentioned bool
```

Update `feishu.VerifyOptions`:

```go
BotOpenID string
```

Extend `messageEnvelope.Event.Message` with `Mentions []messageMention`.

Use a narrow mention struct:

```go
type messageMention struct {
	Key string `json:"key"`
	ID  struct {
		OpenID string `json:"open_id"`
	} `json:"id"`
}
```

When a mention id matches `opts.BotOpenID`, set `BotMentioned = true` and remove the mention key from the beginning of the extracted text. Keep stripping conservative: only remove the matched key if it appears as a leading token.

In `internal/app/app.go`, pass `BotOpenID` into the Feishu receiver verification options.

- [x] **Step 4: Run transport and app tests**

Run: `go test ./internal/transport/feishu ./internal/app -count=1`

Expected: PASS.

- [x] **Step 5: Commit mention metadata**

```bash
git add internal/contracts/contracts.go internal/transport/feishu/normalizer.go internal/transport/feishu/normalizer_test.go internal/app/app.go
git commit -m "feat(feishu): normalize bot mentions"
```

### Task 4: Preserve card callback values for richer actions

**Files:**
- Modify: `internal/contracts/contracts.go`
- Modify: `internal/transport/feishu/normalizer.go`
- Modify: `internal/transport/feishu/normalizer_test.go`

- [x] **Step 1: Write failing callback value test**

Add:

```go
func TestNormalizeCardActionValues(t *testing.T) {
	raw := cardJSONWithValue(t, map[string]any{
		"text": "continue",
		"action": "shortcut",
		"shortcut": "summarize",
		"task_id": "cx_1",
	}, "token_1")
	ev, err := NormalizeCardActionJSON(raw, VerifyOptions{AppID: "cli_test", VerificationToken: "verify"})
	if err != nil {
		t.Fatal(err)
	}
	if ev.ActionValue["action"] != "shortcut" || ev.ActionValue["shortcut"] != "summarize" || ev.Text != "continue" {
		t.Fatalf("unexpected callback values: %+v", ev)
	}
}
```

- [x] **Step 2: Run callback test and verify failure**

Run: `go test ./internal/transport/feishu -run TestNormalizeCardActionValues -count=1`

Expected: FAIL because `ActionValue` does not exist.

- [x] **Step 3: Implement action value normalization**

Add to `contracts.InboundEvent`:

```go
ActionValue map[string]string
```

In `NormalizeCardActionJSON`, decode every string-valued callback value into `ActionValue`. Keep `Text` as the special `text` value for existing continuation behavior. Non-string values should continue to reject only for `text`; ignore non-string auxiliary values unless a test requires stricter validation.

- [x] **Step 4: Run transport tests**

Run: `go test ./internal/transport/feishu -count=1`

Expected: PASS.

- [x] **Step 5: Commit action values**

```bash
git add internal/contracts/contracts.go internal/transport/feishu/normalizer.go internal/transport/feishu/normalizer_test.go
git commit -m "feat(feishu): preserve card action values"
```

## Chunk 3: Store Support For Active Tasks And Pending Project Selection

### Task 5: Add active task lookup

**Files:**
- Modify: `internal/store/store.go`
- Modify: `internal/store/store_test.go`

- [x] **Step 1: Write failing active-task tests**

Add:

```go
func TestFindRunningTaskByChatAndCreator(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	_, err := st.AdmitNewTask(ctx, "evt_1", "message", CreateTaskInput{
		TaskID: "cx_1", RunID: "run_1", CWD: "/repo", CreatedBy: "ou_owner", ChatID: "chat",
		Prompt: "run", EffectiveCodexCommand: "codex", EffectiveSandbox: "workspace-write",
		EffectiveApproval: "never", Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	task, ok, err := st.FindRunningTask(ctx, "chat", "ou_owner")
	if err != nil || !ok || task.ID != "cx_1" {
		t.Fatalf("running task not found task=%+v ok=%v err=%v", task, ok, err)
	}
	if _, ok, err := st.FindRunningTask(ctx, "chat", "ou_other"); err != nil || ok {
		t.Fatalf("other user should not be blocked ok=%v err=%v", ok, err)
	}
}
```

- [x] **Step 2: Run store test and verify failure**

Run: `go test ./internal/store -run TestFindRunningTaskByChatAndCreator -count=1`

Expected: FAIL because `FindRunningTask` does not exist.

- [x] **Step 3: Implement active lookup**

Add:

```go
func (s *Store) FindRunningTask(ctx context.Context, chatID, creatorOpenID string) (Task, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE chat_id=? AND created_by=? AND status='running' ORDER BY created_at DESC LIMIT 1`, chatID, creatorOpenID)
	task, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, false, nil
	}
	if err != nil {
		return Task{}, false, err
	}
	return task, true, nil
}
```

If `scanTask` currently only accepts `*sql.Rows`, add a small `taskScanner` interface shared by `*sql.Row` and `*sql.Rows`.

- [x] **Step 4: Run store tests**

Run: `go test ./internal/store -count=1`

Expected: PASS.

- [x] **Step 5: Commit active lookup**

```bash
git add internal/store/store.go internal/store/store_test.go
git commit -m "feat(store): find active chat tasks"
```

### Task 6: Add pending project selection persistence

**Files:**
- Modify: `internal/store/schema.go`
- Modify: `internal/store/store.go`
- Modify: `internal/store/store_test.go`

- [x] **Step 1: Write failing pending-intent tests**

Add:

```go
func TestPendingIntentCreateConsumeAndExpire(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	pending, err := st.CreatePendingIntent(ctx, CreatePendingIntentInput{
		ID: "pi_1", ChatID: "chat", CreatedBy: "ou_owner", Prompt: "fix tests",
		ProjectAliases: []string{"backend", "frontend"}, Now: now, ExpiresAt: now.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if pending.ID != "pi_1" {
		t.Fatalf("unexpected pending intent: %+v", pending)
	}
	consumed, err := st.ConsumePendingIntent(ctx, "pi_1", "ou_owner", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if consumed.Prompt != "fix tests" {
		t.Fatalf("unexpected consumed intent: %+v", consumed)
	}
	if _, err := st.ConsumePendingIntent(ctx, "pi_1", "ou_owner", now.Add(2*time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected consumed intent to be unavailable, got %v", err)
	}
}
```

- [x] **Step 2: Run pending-intent test and verify failure**

Run: `go test ./internal/store -run TestPendingIntentCreateConsumeAndExpire -count=1`

Expected: FAIL because the table and APIs do not exist.

- [x] **Step 3: Add migration v2 and store APIs**

Increment `migrationVersion` by appending a second migration:

```sql
CREATE TABLE IF NOT EXISTS pending_intents (
	id TEXT PRIMARY KEY,
	chat_id TEXT NOT NULL,
	created_by TEXT NOT NULL,
	prompt TEXT NOT NULL,
	project_aliases_json TEXT NOT NULL DEFAULT '[]',
	status TEXT NOT NULL CHECK (status IN ('pending','consumed','expired')),
	created_at TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	consumed_at TEXT
);

CREATE INDEX IF NOT EXISTS idx_pending_intents_chat_creator ON pending_intents(chat_id, created_by, status);
```

Add types:

```go
type CreatePendingIntentInput struct {
	ID             string
	ChatID         string
	CreatedBy      string
	Prompt         string
	ProjectAliases []string
	Now            time.Time
	ExpiresAt      time.Time
}

type PendingIntent struct {
	ID             string
	ChatID         string
	CreatedBy      string
	Prompt         string
	ProjectAliases []string
}
```

Implement create and consume in transactions. `ConsumePendingIntent` must verify creator, status `pending`, and `expires_at > now`; then set status `consumed`.

- [x] **Step 4: Run store tests**

Run: `go test ./internal/store -count=1`

Expected: PASS.

- [x] **Step 5: Commit pending intents**

```bash
git add internal/store/schema.go internal/store/store.go internal/store/store_test.go
git commit -m "feat(store): persist pending project selections"
```

## Chunk 4: Router Behavior

### Task 7: Route private plain text and remove `/codex` execution

**Files:**
- Modify: `internal/router/router.go`
- Modify: `internal/router/router_test.go`
- Modify: `internal/router/command.go`

- [x] **Step 1: Write failing router tests**

Add tests:

```go
func TestRouterPrivatePlainTextStartsTask(t *testing.T) {
	ctx := context.Background()
	rt, _, runner, notes := newTestRouter(t, []string{"ou_owner"})
	if err := rt.Handle(ctx, contracts.InboundEvent{Kind: contracts.InboundNewTask, DedupKey: "evt_1", ChatType: "private", ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "msg_user", Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	if runner.execCalls != 1 || len(notes.starts) != 1 {
		t.Fatalf("plain private text should start task exec=%d notes=%+v", runner.execCalls, notes)
	}
}

func TestRouterCodexCommandSendsMigrationHint(t *testing.T) {
	ctx := context.Background()
	rt, _, runner, notes := newTestRouter(t, []string{"ou_owner"})
	if err := rt.Handle(ctx, contracts.InboundEvent{Kind: contracts.InboundNewTask, DedupKey: "evt_1", ChatType: "private", ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "msg_user", Text: "/codex hello"}); err != nil {
		t.Fatal(err)
	}
	if runner.execCalls != 0 || len(notes.migrationHints) != 1 {
		t.Fatalf("codex command should not execute exec=%d notes=%+v", runner.execCalls, notes)
	}
}
```

- [x] **Step 2: Run router tests and verify failure**

Run: `go test ./internal/router -run 'TestRouterPrivatePlainTextStartsTask|TestRouterCodexCommandSendsMigrationHint' -count=1`

Expected: FAIL because router still requires `/codex`.

- [x] **Step 3: Implement intent-driven new task path**

Update router to call `intent.ParseStart(...)` for `InboundNewTask`.

Extend `Notifier` interface with:

```go
MigrationHint(ctx context.Context, chatID, replyToMessageID string) error
```

For start intents, reuse existing `ResolveProject`, `AdmitNewTask`, start card, runner, and result flow.

For migration hints, send the hint and return without touching store.

Retire or narrow `ParseCommand`; if kept, tests should reflect that it is no longer the router entry point.

- [x] **Step 4: Run router tests**

Run: `go test ./internal/router -count=1`

Expected: PASS.

- [x] **Step 5: Commit private entry behavior**

```bash
git add internal/router/router.go internal/router/router_test.go internal/router/command.go
git commit -m "feat(router): start private tasks from plain text"
```

### Task 8: Add group project selection and pending callbacks

**Files:**
- Modify: `internal/router/router.go`
- Modify: `internal/router/router_test.go`
- Modify: `internal/notifier/notifier.go`

- [x] **Step 1: Write failing group project tests**

Add tests:

```go
func TestRouterGroupMentionWithoutProjectSendsProjectSelection(t *testing.T) {
	ctx := context.Background()
	rt, _, runner, notes := newTestRouterWithProjects(t, []string{"ou_owner"}, map[string]config.ProjectConfig{
		"backend": {CWD: t.TempDir()},
	})
	if err := rt.Handle(ctx, contracts.InboundEvent{Kind: contracts.InboundNewTask, DedupKey: "evt_1", ChatType: "group", ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "msg_user", Text: "fix tests", BotMentioned: true}); err != nil {
		t.Fatal(err)
	}
	if runner.execCalls != 0 || len(notes.projectSelections) != 1 {
		t.Fatalf("expected project selection exec=%d notes=%+v", runner.execCalls, notes)
	}
}

func TestRouterProjectSelectionStartsPendingTaskOnce(t *testing.T) {
	ctx := context.Background()
	rt, _, runner, notes := newTestRouterWithProjects(t, []string{"ou_owner"}, map[string]config.ProjectConfig{
		"backend": {CWD: t.TempDir()},
	})
	err := rt.Handle(ctx, contracts.InboundEvent{
		Kind: contracts.InboundNewTask, DedupKey: "evt_1", ChatType: "group",
		ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "msg_user",
		Text: "fix tests", BotMentioned: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(notes.projectSelections) != 1 {
		t.Fatalf("expected project selection: %+v", notes)
	}
	pendingID := notes.projectSelections[0].PendingID
	err = rt.Handle(ctx, contracts.InboundEvent{
		Kind: contracts.InboundCardAction, DedupKey: "evt_2", ChatType: "group",
		ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "card_cb",
		ActionID: "project_select",
		ActionValue: map[string]string{"action": "select_project", "pending_id": pendingID, "project": "backend"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if runner.execCalls != 1 {
		t.Fatalf("project selection should start one task, exec=%d", runner.execCalls)
	}
	err = rt.Handle(ctx, contracts.InboundEvent{
		Kind: contracts.InboundCardAction, DedupKey: "evt_3", ChatType: "group",
		ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "card_cb2",
		ActionID: "project_select",
		ActionValue: map[string]string{"action": "select_project", "pending_id": pendingID, "project": "backend"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if runner.execCalls != 1 {
		t.Fatalf("consumed pending intent should not run twice, exec=%d", runner.execCalls)
	}
}
```

- [x] **Step 2: Run group tests and verify failure**

Run: `go test ./internal/router -run 'TestRouterGroupMentionWithoutProjectSendsProjectSelection|TestRouterProjectSelectionStartsPendingTaskOnce' -count=1`

Expected: FAIL because router does not create pending intents or project selection cards.

- [x] **Step 3: Implement group mention and project selection routing**

Extend `TaskStore` interface with pending APIs from Task 6.

Extend `Notifier` with:

```go
ProjectSelection(ctx context.Context, in notify.ProjectSelectionInput) (contracts.SentMessage, error)
```

Handle:

- `KindProjectSelection`: store pending intent with a short id, send project selection card.
- `ActionValue["action"] == "select_project"`: consume pending intent, resolve selected project, create task.
- Unknown project: send project selection card with a short message and do not run.

- [x] **Step 4: Run router tests**

Run: `go test ./internal/router -count=1`

Expected: PASS.

- [x] **Step 5: Commit project selection routing**

```bash
git add internal/router/router.go internal/router/router_test.go internal/notifier/notifier.go
git commit -m "feat(router): add project selection flow"
```

### Task 9: Add running conflict handling

**Files:**
- Modify: `internal/router/router.go`
- Modify: `internal/router/router_test.go`
- Modify: `internal/notifier/notifier.go`

- [x] **Step 1: Write failing running-conflict test**

Add:

```go
func TestRouterRunningConflictDoesNotStartSecondTask(t *testing.T) {
	ctx := context.Background()
	rt, _, runner, notes := newTestRouter(t, []string{"ou_owner"})
	runner.block = true
	if err := rt.Handle(ctx, contracts.InboundEvent{Kind: contracts.InboundNewTask, DedupKey: "evt_1", ChatType: "private", ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "msg_1", Text: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := rt.Handle(ctx, contracts.InboundEvent{Kind: contracts.InboundNewTask, DedupKey: "evt_2", ChatType: "private", ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "msg_2", Text: "second"}); err != nil {
		t.Fatal(err)
	}
	if runner.execCalls != 1 || len(notes.runningConflicts) != 1 {
		t.Fatalf("second task should be blocked exec=%d notes=%+v", runner.execCalls, notes)
	}
}
```

The fake runner can simulate a task remaining running by not calling finish until after the second event, or the test can pre-seed a running task through the store.

- [x] **Step 2: Run conflict test and verify failure**

Run: `go test ./internal/router -run TestRouterRunningConflictDoesNotStartSecondTask -count=1`

Expected: FAIL because router does not check active tasks before admission.

- [x] **Step 3: Implement conflict check**

Before `AdmitNewTask`, call `FindRunningTask(chatID, senderOpenID)`. If found, call:

```go
RunningConflict(ctx context.Context, in notify.RunningConflictInput) error
```

Do not insert a dedup row for conflict messages unless store design explicitly tracks rejected events. Keep the behavior idempotent by relying on Feishu event id and no side effects besides the reply card.

- [x] **Step 4: Run router tests**

Run: `go test ./internal/router -count=1`

Expected: PASS.

- [x] **Step 5: Commit conflict handling**

```bash
git add internal/router/router.go internal/router/router_test.go internal/notifier/notifier.go
git commit -m "feat(router): report running task conflicts"
```

### Task 10: Add shortcut and confirmation routing

**Files:**
- Modify: `internal/router/router.go`
- Modify: `internal/router/router_test.go`
- Modify: `internal/notifier/notifier.go`

- [x] **Step 1: Write failing shortcut tests**

Add tests:

```go
func TestRouterShortcutSummarizeResumesImmediately(t *testing.T) {
	ctx := context.Background()
	rt, _, runner, _ := newTestRouter(t, []string{"ou_owner"})
	startTaskForShortcutTest(t, ctx, rt)
	err := rt.Handle(ctx, contracts.InboundEvent{
		Kind: contracts.InboundCardAction, DedupKey: "shortcut_1", ChatType: "private",
		ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "card_cb",
		RootMessageID: "msg_result", ActionID: "shortcut",
		ActionValue: map[string]string{"shortcut": "summarize"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if runner.resumeCalls != 1 || !strings.Contains(runner.lastResumeReply, "Summarize") {
		t.Fatalf("unexpected shortcut resume calls=%d reply=%q", runner.resumeCalls, runner.lastResumeReply)
	}
}

func TestRouterShortcutRunTestsRequiresConfirmation(t *testing.T) {
	ctx := context.Background()
	rt, _, runner, notes := newTestRouter(t, []string{"ou_owner"})
	startTaskForShortcutTest(t, ctx, rt)
	err := rt.Handle(ctx, contracts.InboundEvent{
		Kind: contracts.InboundCardAction, DedupKey: "shortcut_1", ChatType: "private",
		ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "card_cb",
		RootMessageID: "msg_result", ActionID: "shortcut",
		ActionValue: map[string]string{"shortcut": "run_tests"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if runner.resumeCalls != 0 || len(notes.confirmations) != 1 {
		t.Fatalf("run tests should require confirmation resumes=%d notes=%+v", runner.resumeCalls, notes)
	}
}
```

- [x] **Step 2: Run shortcut tests and verify failure**

Run: `go test ./internal/router -run 'TestRouterShortcutSummarizeResumesImmediately|TestRouterShortcutRunTestsRequiresConfirmation' -count=1`

Expected: FAIL because shortcuts are not routed.

- [x] **Step 3: Implement shortcut routing**

Add shortcut prompt mapping in router or a small `internal/intent/shortcuts.go`:

```go
var ShortcutPrompts = map[string]Shortcut{
	"summarize": {Prompt: "Summarize the current task result and next steps.", Immediate: true},
	"explain_error": {Prompt: "Explain the latest error and suggest the next debugging step.", Immediate: true},
	"run_tests": {Prompt: "Run the relevant tests for the current change and report the result.", Immediate: false},
	"mr_description": {Prompt: "Draft a concise merge request description for the current change.", Immediate: false},
}
```

Immediate shortcuts call the existing resume path with the preset prompt. Confirmed shortcuts send a confirmation card with action values `{action: confirm_shortcut, shortcut: run_tests}`. Confirmed callback then resumes.

- [x] **Step 4: Run router tests**

Run: `go test ./internal/router -count=1`

Expected: PASS.

- [x] **Step 5: Commit shortcut routing**

```bash
git add internal/router/router.go internal/router/router_test.go internal/notifier/notifier.go internal/intent
git commit -m "feat(router): add task shortcut actions"
```

## Chunk 5: Compact Cards And Notifier APIs

### Task 11: Add structured notifier card inputs

**Files:**
- Modify: `internal/contracts/contracts.go`
- Modify: `internal/notifier/notifier.go`
- Modify: `internal/notifier/notifier_test.go`

- [x] **Step 1: Write failing notifier tests**

Add tests asserting:

- start/success/failure cards contain compact status metadata;
- project selection card contains project aliases and action values;
- running conflict card includes current task id and project;
- shortcut confirmation card includes execute and cancel actions;
- redaction still applies to body, workspace labels, and titles.

Example:

```go
func TestProjectSelectionCardActions(t *testing.T) {
	sender := &fakeSender{messageID: "msg_project"}
	n := New(sender)
	_, err := n.ProjectSelection(context.Background(), ProjectSelectionInput{
		ChatID: "chat", ReplyToMessageID: "msg_user", PendingID: "pi_1",
		Prompt: "fix tests", ProjectAliases: []string{"backend", "frontend"},
	})
	if err != nil {
		t.Fatal(err)
	}
	msg := sender.messages[0]
	if msg.CardKind != contracts.CardProjectSelection || len(msg.Actions) != 2 {
		t.Fatalf("unexpected project selection: %+v", msg)
	}
	if msg.Actions[0].Value["action"] != "select_project" || msg.Actions[0].Value["pending_id"] != "pi_1" {
		t.Fatalf("missing action payload: %+v", msg.Actions[0])
	}
}
```

- [x] **Step 2: Run notifier tests and verify failure**

Run: `go test ./internal/notifier -count=1`

Expected: FAIL because new card kinds and action payloads do not exist.

- [x] **Step 3: Extend contracts and notifier**

Add card kinds:

```go
const (
	CardStart CardKind = "start"
	CardSuccess CardKind = "success"
	CardFailure CardKind = "failure"
	CardRoutingError CardKind = "routing_error"
	CardProjectSelection CardKind = "project_selection"
	CardRunningConflict CardKind = "running_conflict"
	CardMigrationHint CardKind = "migration_hint"
	CardShortcutConfirm CardKind = "shortcut_confirm"
)
```

Extend action:

```go
type Action struct {
	ID    string
	Label string
	Style string
	Value map[string]string
}
```

Add outbound metadata:

```go
Fields []Field
```

where `Field` is a simple title/value pair.

Implement notifier methods for project selection, running conflict, migration hint, and shortcut confirmation. Keep redaction in notifier before data reaches sender.

- [x] **Step 4: Run notifier tests**

Run: `go test ./internal/notifier -count=1`

Expected: PASS.

- [x] **Step 5: Commit notifier APIs**

```bash
git add internal/contracts/contracts.go internal/notifier/notifier.go internal/notifier/notifier_test.go
git commit -m "feat(notifier): add compact task cards"
```

### Task 12: Render compact Feishu interactive cards

**Files:**
- Modify: `internal/transport/feishu/sender.go`
- Modify: `internal/transport/feishu/sender_test.go`

- [x] **Step 1: Write failing card renderer tests**

Add tests that unmarshal card JSON and assert:

- `header.template` or equivalent color/template is set per status;
- metadata fields render;
- multiple buttons render;
- each button carries its `value` map plus `action_id`;
- continue cards still include multiline input when configured.

Example:

```go
func TestBuildInteractiveCardWithActionValues(t *testing.T) {
	card, err := BuildInteractiveCard(contracts.OutboundMessage{
		CardKind: contracts.CardProjectSelection,
		Title: "Choose project",
		BodyMarkdown: "Select a project.",
		Actions: []contracts.Action{
			{ID: "project_select", Label: "backend", Value: map[string]string{"action": "select_project", "project": "backend", "pending_id": "pi_1"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(card, &decoded); err != nil {
		t.Fatalf("invalid card json: %v", err)
	}
	if !jsonContains(string(card), "select_project") || !jsonContains(string(card), "backend") {
		t.Fatalf("card missing action values: %s", string(card))
	}
}
```

- [x] **Step 2: Run sender tests and verify failure**

Run: `go test ./internal/transport/feishu -run TestBuildInteractiveCardWithActionValues -count=1`

Expected: FAIL because action values and richer card blocks are not rendered.

- [x] **Step 3: Implement renderer support**

Update `BuildInteractiveCard` to:

- choose header template based on `CardKind` or `Status`;
- render `Fields` as compact markdown or Feishu field elements;
- render all `Actions`, not only `Actions[0]`;
- merge `Action.Value` with `action_id`;
- preserve the existing multiline input for continuation cards;
- avoid nested, large, or decorative card structures.

- [x] **Step 4: Run Feishu transport tests**

Run: `go test ./internal/transport/feishu -count=1`

Expected: PASS.

- [x] **Step 5: Commit card renderer**

```bash
git add internal/transport/feishu/sender.go internal/transport/feishu/sender_test.go
git commit -m "feat(feishu): render compact action cards"
```

## Chunk 6: Integration, Documentation, And Final Verification

### Task 13: Update app and integration flows

**Files:**
- Modify: `internal/app/integration_test.go`
- Modify: `internal/app/app_test.go`
- Modify: `internal/router/router_test.go`

- [x] **Step 1: Update failing integration tests first**

Change existing integration new-task inputs from `/codex hello` to `hello`. Add new integration cases:

```go
t.Run("private plain task", ...)
t.Run("group mention project selection then task", ...)
t.Run("task card reply resume still routes by message id", ...)
t.Run("route miss never falls back to latest task", ...)
```

- [x] **Step 2: Run integration tests and verify any remaining failures**

Run: `go test ./internal/app ./internal/router -count=1`

Expected: FAIL only where implementation gaps remain from previous chunks; after prior chunks are complete it should PASS.

- [x] **Step 3: Fix integration wiring**

Wire the new config `BotOpenID`, parser, notifier methods, and store methods through `app.Serve` and test fakes. Keep fake senders/receivers simple and deterministic.

- [x] **Step 4: Run integration tests**

Run: `go test ./internal/app ./internal/router -count=1`

Expected: PASS.

- [x] **Step 5: Commit integration updates**

```bash
git add internal/app/integration_test.go internal/app/app_test.go internal/router/router_test.go
git commit -m "test(app): cover improved Feishu UX flows"
```

### Task 14: Update user documentation

**Files:**
- Modify: `README.md`
- Modify: `README.zh-CN.md`
- Modify: `docs/feishu-quickstart.zh-CN.md`
- Modify: `docs/feishu-setup.md`
- Modify: `docs/troubleshooting.md`

- [x] **Step 1: Update docs examples**

Replace `/codex` examples with:

```text
fix the failing router test
@backend fix the failing router test
```

For group chat, document:

```text
@Codex @backend fix the failing router test
```

Document that `@Codex fix the failing router test` returns a project selection card.

- [x] **Step 2: Document old command behavior**

State that `/codex` is no longer the task entry point and will return a migration hint.

- [x] **Step 3: Document shortcut buttons**

List:

- Continue: free-form follow-up.
- Summarize: immediate resume.
- Explain error: immediate resume.
- Run tests: confirmation required.
- MR description: confirmation required.

- [x] **Step 4: Run docs-sensitive tests**

Run: `make test`

Expected: PASS, including `scripts/test-init-local-config.sh`.

- [x] **Step 5: Commit documentation**

```bash
git add README.md README.zh-CN.md docs/feishu-quickstart.zh-CN.md docs/feishu-setup.md docs/troubleshooting.md
git commit -m "docs: update Feishu chat usage"
```

### Task 15: Final verification and cleanup

**Files:**
- All changed files

- [x] **Step 1: Run formatting**

Run: `gofmt -w internal/intent/*.go internal/contracts/contracts.go internal/config/config.go internal/transport/feishu/*.go internal/store/*.go internal/router/*.go internal/notifier/*.go internal/app/*.go`

Expected: command exits 0 and only intended Go files are formatted.

- [x] **Step 2: Run full test suite**

Run: `make test`

Expected: all packages PASS and `scripts/test-init-local-config.sh` PASS.

- [x] **Step 3: Run vet**

Run: `go vet ./...`

Expected: no output, exit 0.

- [x] **Step 4: Inspect diff**

Run: `git status --short && git diff --stat origin/dev...HEAD`

Expected: no unstaged files; diff contains only the planned UX, parser, store, notifier, renderer, integration, and docs changes.

- [x] **Step 5: Prepare final PR or merge workflow**

Use repository workflow requested by the user. If pushing to GitHub over normal git HTTPS fails, use the same GitHub API fallback pattern already proven in this repo only after confirming local and remote base SHAs match.
