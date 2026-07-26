package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFreshSchemaAndTaskLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	defer func() { _ = s.Close() }()
	for _, table := range []string{"schema_migrations", "tasks", "runs", "message_routes", "event_dedup", "users"} {
		if !tableExists(t, s.db, table) {
			t.Fatalf("missing table %s", table)
		}
	}
	if tableExists(t, s.db, "pending_intents") {
		t.Fatal("fresh schema must not contain retired pending_intents table")
	}
	var version int
	if err := s.db.QueryRow(`SELECT version FROM schema_migrations`).Scan(&version); err != nil || version != migrationVersion {
		t.Fatalf("schema migration version = %d, want %d (err=%v)", version, migrationVersion, err)
	}
	for _, column := range []string{"codex_thread_id", "status", "root_message_id"} {
		if !columnExists(t, s.db, "tasks", column) {
			t.Fatalf("tasks.%s missing", column)
		}
	}
	if columnExists(t, s.db, "tasks", "codex_session_id") {
		t.Fatal("legacy codex_session_id should not exist in fresh schema")
	}

	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	admit, err := s.AdmitNewTask(ctx, "new-1", "message", CreateTaskInput{
		TaskID: "task-1", RunID: "run-1", ProjectAlias: "backend", CWD: "/repo", CreatedBy: "ou_owner", ChatID: "chat", Prompt: "fix tests", Now: now,
	})
	if err != nil || admit.Replay || admit.Task.Status != "queued" || admit.Run.Kind != "new" || admit.Run.Status != "queued" {
		t.Fatalf("unexpected new admission: %+v err=%v", admit, err)
	}
	task, run, err := s.StartRun(ctx, StartRunInput{RunID: admit.Run.ID, ThreadID: "thread-1", TurnID: "turn-1", Now: now})
	if err != nil || task.Status != "running" || task.CodexThreadID != "thread-1" || run.CodexTurnID != "turn-1" {
		t.Fatalf("start run failed task=%+v run=%+v err=%v", task, run, err)
	}
	if err := s.FinishRun(ctx, "new-1", FinishRunInput{RunID: run.ID, ThreadID: "thread-1", TurnID: "turn-1", Status: "succeeded", ExitCode: 0, FinalText: "done", FinishedAt: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	task, runs, err := s.GetTask(ctx, "task-1")
	if err != nil || task.Status != "succeeded" || task.CodexThreadID != "thread-1" || len(runs) != 1 || runs[0].FinalText != "done" {
		t.Fatalf("unexpected finished task=%+v runs=%+v err=%v", task, runs, err)
	}

	resume, err := s.AdmitResumeRun(ctx, "resume-1", "message", ResumeRunInput{RunID: "run-2", TaskID: task.ID, RequestedBy: "ou_owner", Prompt: "continue", Now: now.Add(2 * time.Minute)})
	if err != nil || resume.Reason != RejectNone || resume.Run.Kind != "resume" || resume.Run.CodexThreadID != "thread-1" {
		t.Fatalf("unexpected resume admission: %+v err=%v", resume, err)
	}
	if duplicate, err := s.AdmitResumeRun(ctx, "resume-2", "message", ResumeRunInput{RunID: "run-3", TaskID: task.ID, RequestedBy: "ou_owner", Prompt: "again", Now: now}); err != nil || duplicate.Reason != RejectActiveRun {
		t.Fatalf("active run should reject: %+v err=%v", duplicate, err)
	}
	if _, _, err := s.StartRun(ctx, StartRunInput{RunID: resume.Run.ID, ThreadID: "thread-1", TurnID: "turn-2", Now: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun(ctx, "resume-1", FinishRunInput{RunID: resume.Run.ID, ThreadID: "thread-1", TurnID: "turn-2", Status: "canceled", ExitCode: -1, FinalText: "stopped", FinishedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.StartRun(ctx, StartRunInput{RunID: resume.Run.ID, ThreadID: "thread-1", TurnID: "turn-2", Now: now}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("terminal run must not restart, got %v", err)
	}
}

func TestAttachThreadAndMessageRoutes(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	defer func() { _ = s.Close() }()
	now := time.Now().UTC()
	task, replay, err := s.AttachThread(ctx, "attach-1", "card_callback", AttachThreadInput{
		TaskID: "task-attached", ThreadID: "desktop-thread", CWD: "/desktop/repo", CreatedBy: "ou_owner", ChatID: "chat", Now: now,
	})
	if err != nil || replay || task.Status != "idle" || task.CodexThreadID != "desktop-thread" {
		t.Fatalf("unexpected attached task: %+v replay=%v err=%v", task, replay, err)
	}
	if err := s.SetTaskRootMessageID(ctx, task.ID, "message-1", now); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertMessageRoute(ctx, "message-1", task.ID, "start_card"); err != nil {
		t.Fatal(err)
	}
	routed, err := s.ResolveMessageRoute(ctx, "message-1")
	if err != nil || routed.ID != task.ID || routed.RootMessageID != "message-1" {
		t.Fatalf("unexpected route: %+v err=%v", routed, err)
	}
	if _, replay, err := s.AttachThread(ctx, "attach-1", "card_callback", AttachThreadInput{TaskID: "other", ThreadID: "other", CWD: "/repo", CreatedBy: "ou_owner", ChatID: "chat", Now: now}); err != nil || !replay {
		t.Fatalf("dedup replay = %v err=%v", replay, err)
	}
	if _, err := s.AdmitResumeRun(ctx, "bad-owner", "message", ResumeRunInput{RunID: "run", TaskID: task.ID, RequestedBy: "ou_other", Prompt: "x", Now: now}); err != nil {
		t.Fatal(err)
	} else if result, err := s.AdmitResumeRun(ctx, "missing-thread", "message", ResumeRunInput{RunID: "run2", TaskID: "missing", RequestedBy: "ou_owner", Prompt: "x", Now: now}); err != nil || result.Reason != RejectRouteMiss {
		t.Fatalf("missing task result=%+v err=%v", result, err)
	}
}

func TestRecoverRunningMarksTasksTerminal(t *testing.T) {
	ctx := context.Background()
	s := openRunningTask(t, ctx)
	defer func() { _ = s.Close() }()
	now := time.Now().UTC()
	if err := s.RecoverRunning(ctx, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	task, runs, err := s.GetTask(ctx, "task")
	if err != nil || task.Status != "failed" || runs[0].Status != "failed" {
		t.Fatalf("recovery task=%+v runs=%+v err=%v", task, runs, err)
	}
}

func TestRejectsUnsupportedMultiVersionDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	createUnsupportedMultiVersionDatabase(t, path)
	if _, err := Open(ctx, path); err == nil || !strings.Contains(err.Error(), "existing state database is not supported") {
		t.Fatalf("legacy state should be rejected, got %v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if tableExists(t, db, "pending_intents") {
		t.Fatal("legacy database should be rejected before new tables are created")
	}
}

func TestRejectsPreviousStateSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "previous.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL); INSERT INTO schema_migrations(version,applied_at) VALUES(1,'2026-01-01T00:00:00Z');`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, path); err == nil || !strings.Contains(err.Error(), "existing state database is not supported") {
		t.Fatalf("previous state schema should be rejected, got %v", err)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func openRunningTask(t *testing.T, ctx context.Context) *Store {
	t.Helper()
	s := openTestStore(t)
	now := time.Now().UTC()
	admit, err := s.AdmitNewTask(ctx, "new", "message", CreateTaskInput{TaskID: "task", RunID: "run", CWD: "/repo", CreatedBy: "ou_owner", ChatID: "chat", Prompt: "work", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.StartRun(ctx, StartRunInput{RunID: admit.Run.ID, ThreadID: "thread", TurnID: "turn", Now: now}); err != nil {
		t.Fatal(err)
	}
	return s
}

func createUnsupportedMultiVersionDatabase(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	legacy := `
CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);
CREATE TABLE tasks (
 id TEXT PRIMARY KEY, codex_session_id TEXT, status TEXT NOT NULL, project_alias TEXT NOT NULL, cwd TEXT NOT NULL,
 created_by TEXT NOT NULL, chat_id TEXT NOT NULL, root_message_id TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
INSERT INTO schema_migrations(version,applied_at) VALUES(1,'2026-01-01T00:00:00Z'),(2,'2026-01-01T00:00:00Z');
`
	if _, err := db.Exec(legacy); err != nil {
		t.Fatal(err)
	}
}

func tableExists(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()
	var name string
	return db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name) == nil
}

func columnExists(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, typ string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if name == column {
			return true
		}
	}
	return false
}
