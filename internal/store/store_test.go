package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/sihuo/codex-feishu-bridge/internal/contracts"
)

func TestMigrationsCreateSpecSchema(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	defer s.Close()

	for _, table := range []string{"schema_migrations", "tasks", "message_routes", "runs", "event_dedup", "users"} {
		if !tableExists(t, s.db, table) {
			t.Fatalf("missing table %s", table)
		}
	}
	for table, cols := range map[string][]string{
		"tasks":          {"id", "codex_session_id", "status", "project_alias", "cwd", "created_by", "chat_id", "root_message_id", "effective_codex_command", "effective_sandbox", "effective_model", "effective_approval", "effective_approval_flag_supported", "effective_extra_args_json", "created_at", "updated_at"},
		"message_routes": {"feishu_message_id", "task_id", "route_type", "created_at"},
		"runs":           {"id", "task_id", "kind", "status", "prompt", "codex_session_id", "exit_code", "started_at", "finished_at", "log_path", "final_text"},
		"event_dedup":    {"dedup_key", "received_at", "source", "state", "task_id", "run_id", "completed_at", "last_error"},
		"users":          {"feishu_open_id", "role", "enabled"},
	} {
		for _, col := range cols {
			if !columnExists(t, s.db, table, col) {
				t.Fatalf("missing column %s.%s", table, col)
			}
		}
	}
	for _, index := range []string{"idx_tasks_codex_session_id", "idx_runs_task_id", "idx_runs_status", "idx_message_routes_task_id", "runs_one_active_per_task"} {
		if !indexExists(t, s.db, index) {
			t.Fatalf("missing index %s", index)
		}
	}
	var fkEnabled int
	if err := s.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fkEnabled); err != nil {
		t.Fatal(err)
	}
	if fkEnabled != 1 {
		t.Fatalf("foreign_keys = %d, want 1", fkEnabled)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO tasks(id,status,project_alias,cwd,created_by,chat_id,effective_codex_command,effective_sandbox,effective_approval,effective_approval_flag_supported,effective_extra_args_json,created_at,updated_at) VALUES('t','invalid','','','','','codex','workspace-write','never',0,'[]','now','now')`); err == nil {
		t.Fatal("expected status check constraint failure")
	}
}

func TestAdmitNewTaskCreatesTaskRunAndDedupAtomically(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	defer s.Close()
	now := time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC)

	res, err := s.AdmitNewTask(ctx, "event-1", "message", CreateTaskInput{
		TaskID:                         "cx_123",
		RunID:                          "run_1",
		ProjectAlias:                   "backend",
		CWD:                            "/repo/backend",
		CreatedBy:                      "ou_owner",
		ChatID:                         "chat_1",
		Prompt:                         "fix bug",
		EffectiveCodexCommand:          "codex",
		EffectiveSandbox:               "workspace-write",
		EffectiveModel:                 "gpt-5",
		EffectiveApproval:              "never",
		EffectiveApprovalFlagSupported: true,
		EffectiveExtraArgs:             []string{"--skip-git-repo-check"},
		Now:                            now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Replay || res.Task.ID != "cx_123" || res.Run.ID != "run_1" || res.Task.Status != "running" || res.Run.Status != "running" {
		t.Fatalf("unexpected admit result: %+v", res)
	}
	if len(res.Task.EffectiveExtraArgs) != 1 || res.Task.EffectiveExtraArgs[0] != "--skip-git-repo-check" {
		t.Fatalf("extra args not decoded: %+v", res.Task)
	}
	replay, err := s.AdmitNewTask(ctx, "event-1", "message", CreateTaskInput{TaskID: "cx_456", RunID: "run_2", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Replay || replay.Reason != RejectReplay {
		t.Fatalf("expected replay rejection, got %+v", replay)
	}
}

func TestAdmitResumeRunCreatorAndSessionGuards(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	defer s.Close()
	now := time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC)
	task := createFinishedTask(t, ctx, s, now, "thread_1")

	res, err := s.AdmitResumeRun(ctx, "resume-1", "message", ResumeRunInput{RunID: "run_2", TaskID: task.ID, RequestedBy: "ou_other", Prompt: "continue", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if res.Reason != RejectCreatorMismatch {
		t.Fatalf("expected creator mismatch, got %+v", res)
	}
	res, err = s.AdmitResumeRun(ctx, "resume-2", "message", ResumeRunInput{RunID: "run_3", TaskID: task.ID, RequestedBy: "ou_owner", Prompt: "continue", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if res.Reason != RejectNone || res.Run.Kind != "resume" || res.Task.CodexSessionID != "thread_1" {
		t.Fatalf("unexpected resume admission: %+v", res)
	}
	active, err := s.AdmitResumeRun(ctx, "resume-3", "message", ResumeRunInput{RunID: "run_4", TaskID: task.ID, RequestedBy: "ou_owner", Prompt: "again", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if active.Reason != RejectActiveRun {
		t.Fatalf("expected active run rejection, got %+v", active)
	}

	missing := createFinishedTask(t, ctx, s, now, "")
	res, err = s.AdmitResumeRun(ctx, "resume-4", "message", ResumeRunInput{RunID: "run_5", TaskID: missing.ID, RequestedBy: "ou_owner", Prompt: "continue", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if res.Reason != RejectMissingSession {
		t.Fatalf("expected missing session, got %+v", res)
	}
}

func TestRecordRunSessionFinishRoutesUsersAndRecovery(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	defer s.Close()
	now := time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC)
	res, err := s.AdmitNewTask(ctx, "event-1", "message", CreateTaskInput{
		TaskID:                "cx_123",
		RunID:                 "run_1",
		CWD:                   "/repo",
		CreatedBy:             "ou_owner",
		ChatID:                "chat_1",
		Prompt:                "hello",
		EffectiveCodexCommand: "codex",
		EffectiveSandbox:      "workspace-write",
		EffectiveApproval:     "never",
		Now:                   now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRunSession(ctx, res.Run.ID, "thread_1"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRunSession(ctx, res.Run.ID, "thread_1"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun(ctx, "event-1", res.Run.ID, contracts.RunResult{CodexSessionID: "thread_1", ExitCode: 0, LogPath: "/log", FinalText: "done", StartedAt: now, FinishedAt: now.Add(time.Minute)}, "succeeded"); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertMessageRoute(ctx, "msg_start", res.Task.ID, "start_card"); err != nil {
		t.Fatal(err)
	}
	task, err := s.ResolveMessageRoute(ctx, "msg_start")
	if err != nil {
		t.Fatal(err)
	}
	if task.ID != res.Task.ID || task.CodexSessionID != "thread_1" {
		t.Fatalf("unexpected routed task: %+v", task)
	}
	if _, err := s.ResolveMessageRoute(ctx, "missing"); !errors.Is(err, ErrRouteMiss) {
		t.Fatalf("expected route miss, got %v", err)
	}
	if err := s.RefreshUsers(ctx, []string{"ou_owner", "ou_new"}); err != nil {
		t.Fatal(err)
	}
	ok, err := s.UserEnabled(ctx, "ou_owner")
	if err != nil || !ok {
		t.Fatalf("expected enabled user, ok=%v err=%v", ok, err)
	}
	if err := s.RefreshUsers(ctx, []string{"ou_new"}); err != nil {
		t.Fatal(err)
	}
	ok, err = s.UserEnabled(ctx, "ou_owner")
	if err != nil || ok {
		t.Fatalf("expected disabled user, ok=%v err=%v", ok, err)
	}

	res2, err := s.AdmitNewTask(ctx, "event-2", "message", CreateTaskInput{TaskID: "cx_running", RunID: "run_running", CWD: "/repo", CreatedBy: "ou_owner", ChatID: "chat", Prompt: "x", EffectiveCodexCommand: "codex", EffectiveSandbox: "workspace-write", EffectiveApproval: "never", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverRunning(ctx, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, runs, err := s.GetTask(ctx, res2.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "failed" || runs[0].Status != "failed" {
		t.Fatalf("recovery did not fail running task/run: task=%+v runs=%+v", got, runs)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func createFinishedTask(t *testing.T, ctx context.Context, s *Store, now time.Time, sessionID string) Task {
	t.Helper()
	id := "cx_missing"
	runID := "run_missing"
	if sessionID != "" {
		id = "cx_123"
		runID = "run_1"
	}
	res, err := s.AdmitNewTask(ctx, "new-"+id, "message", CreateTaskInput{TaskID: id, RunID: runID, CWD: "/repo", CreatedBy: "ou_owner", ChatID: "chat", Prompt: "hello", EffectiveCodexCommand: "codex", EffectiveSandbox: "workspace-write", EffectiveApproval: "never", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	result := contracts.RunResult{CodexSessionID: sessionID, ExitCode: 0, LogPath: "/log", FinalText: "done", StartedAt: now, FinishedAt: now.Add(time.Minute)}
	if err := s.FinishRun(ctx, "new-"+id, res.Run.ID, result, "succeeded"); err != nil {
		t.Fatal(err)
	}
	task, _, err := s.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func tableExists(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()
	var name string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
	return err == nil
}

func columnExists(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			t.Fatal(err)
		}
		if name == column {
			return true
		}
	}
	return false
}

func indexExists(t *testing.T, db *sql.DB, index string) bool {
	t.Helper()
	var name string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, index).Scan(&name)
	return err == nil
}
