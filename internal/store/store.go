package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrRouteMiss = errors.New("message route not found")
	ErrNotFound  = errors.New("not found")
)

type Store struct {
	db *sql.DB
}

type RejectionReason string

const (
	RejectNone            RejectionReason = ""
	RejectReplay          RejectionReason = "replay"
	RejectCreatorMismatch RejectionReason = "creator_mismatch"
	RejectRouteMiss       RejectionReason = "route_miss"
	RejectStatus          RejectionReason = "status_rejected"
	RejectActiveRun       RejectionReason = "active_run"
	RejectMissingThread   RejectionReason = "missing_thread"
)

type CreateTaskInput struct {
	TaskID       string
	RunID        string
	ProjectAlias string
	CWD          string
	CreatedBy    string
	ChatID       string
	Prompt       string
	Now          time.Time
}

type AttachThreadInput struct {
	TaskID       string
	ThreadID     string
	ProjectAlias string
	CWD          string
	CreatedBy    string
	ChatID       string
	Now          time.Time
}

type ResumeRunInput struct {
	RunID       string
	TaskID      string
	RequestedBy string
	Prompt      string
	Now         time.Time
}

type StartRunInput struct {
	RunID    string
	ThreadID string
	TurnID   string
	Now      time.Time
}

type FinishRunInput struct {
	RunID      string
	ThreadID   string
	TurnID     string
	Status     string
	ExitCode   int
	FinalText  string
	FinishedAt time.Time
}

type AdmitResult struct {
	Replay bool
	Reason RejectionReason
	Task   Task
	Run    Run
}

type Task struct {
	ID            string
	CodexThreadID string
	Status        string
	ProjectAlias  string
	CWD           string
	CreatedBy     string
	ChatID        string
	RootMessageID string
}

type Run struct {
	ID            string
	TaskID        string
	Kind          string
	Status        string
	Prompt        string
	CodexThreadID string
	CodexTurnID   string
	ExitCode      int
	FinalText     string
}

func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("store path is required")
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate(ctx context.Context) error {
	hasMigrations, err := s.hasTable(ctx, "schema_migrations")
	if err != nil {
		return err
	}
	if hasMigrations {
		var count, version int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&count, &version); err != nil {
			return err
		}
		hasApprovals, err := s.hasTable(ctx, "approvals")
		if err != nil {
			return err
		}
		if count != 1 || version != migrationVersion || hasApprovals {
			return errors.New("existing state database is not supported; remove it and start the bridge with a fresh state database")
		}
		return nil
	}
	hasLegacyState, err := s.hasState(ctx)
	if err != nil {
		return err
	}
	if hasLegacyState {
		return errors.New("existing state database is not supported; remove it and start the bridge with a fresh state database")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if _, err := tx.ExecContext(ctx, schema); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,applied_at) VALUES(?,?)`, migrationVersion, formatTime(time.Now().UTC())); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) hasState(ctx context.Context) (bool, error) {
	var name string
	// Include retired tables so an older state database is rejected rather than
	// being mistaken for a fresh database.
	err := s.db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name IN ('tasks','runs','message_routes','event_dedup','users','pending_intents','approvals') LIMIT 1`).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) hasTable(ctx context.Context, table string) (bool, error) {
	var name string
	err := s.db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) AdmitNewTask(ctx context.Context, dedupKey, source string, in CreateTaskInput) (AdmitResult, error) {
	now := normalizeTime(in.Now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AdmitResult{}, err
	}
	defer rollback(tx)
	if inserted, err := insertDedup(ctx, tx, dedupKey, source, now); err != nil {
		return AdmitResult{}, err
	} else if !inserted {
		return AdmitResult{Replay: true, Reason: RejectReplay}, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO tasks(id,status,project_alias,cwd,created_by,chat_id,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?)`,
		in.TaskID, "queued", in.ProjectAlias, in.CWD, in.CreatedBy, in.ChatID, formatTime(now), formatTime(now)); err != nil {
		return AdmitResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO runs(id,task_id,kind,status,prompt,started_at)
VALUES(?,?,?,?,?,?)`, in.RunID, in.TaskID, "new", "queued", in.Prompt, formatTime(now)); err != nil {
		return AdmitResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE event_dedup SET task_id=?,run_id=? WHERE dedup_key=?`, in.TaskID, in.RunID, dedupKey); err != nil {
		return AdmitResult{}, err
	}
	task, err := getTaskTx(ctx, tx, in.TaskID)
	if err != nil {
		return AdmitResult{}, err
	}
	run, err := getRunTx(ctx, tx, in.RunID)
	if err != nil {
		return AdmitResult{}, err
	}
	return AdmitResult{Task: task, Run: run}, tx.Commit()
}

func (s *Store) AttachThread(ctx context.Context, dedupKey, source string, in AttachThreadInput) (Task, bool, error) {
	now := normalizeTime(in.Now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, false, err
	}
	defer rollback(tx)
	if inserted, err := insertDedup(ctx, tx, dedupKey, source, now); err != nil {
		return Task{}, false, err
	} else if !inserted {
		return Task{}, true, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO tasks(id,codex_thread_id,status,project_alias,cwd,created_by,chat_id,created_at,updated_at)
VALUES(?,?, 'idle', ?,?,?,?,?,?)`,
		in.TaskID, in.ThreadID, in.ProjectAlias, in.CWD, in.CreatedBy, in.ChatID, formatTime(now), formatTime(now)); err != nil {
		return Task{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE event_dedup SET state='completed',task_id=?,completed_at=? WHERE dedup_key=?`, in.TaskID, formatTime(now), dedupKey); err != nil {
		return Task{}, false, err
	}
	task, err := getTaskTx(ctx, tx, in.TaskID)
	if err != nil {
		return Task{}, false, err
	}
	return task, false, tx.Commit()
}

// AdmitRestart records a native service restart command as complete before the
// caller tears down the process. A redelivered Feishu event must not cause a
// second restart after the supervisor brings the bridge back up.
func (s *Store) AdmitRestart(ctx context.Context, dedupKey, source string, now time.Time) (bool, error) {
	now = normalizeTime(now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer rollback(tx)
	inserted, err := insertDedup(ctx, tx, dedupKey, source, now)
	if err != nil {
		return false, err
	}
	if !inserted {
		return false, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE event_dedup SET state='completed',completed_at=? WHERE dedup_key=?`, formatTime(now), dedupKey); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *Store) AdmitResumeRun(ctx context.Context, dedupKey, source string, in ResumeRunInput) (AdmitResult, error) {
	now := normalizeTime(in.Now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AdmitResult{}, err
	}
	defer rollback(tx)
	if inserted, err := insertDedup(ctx, tx, dedupKey, source, now); err != nil {
		return AdmitResult{}, err
	} else if !inserted {
		return AdmitResult{Replay: true, Reason: RejectReplay}, tx.Commit()
	}
	task, err := getTaskTx(ctx, tx, in.TaskID)
	if errors.Is(err, sql.ErrNoRows) {
		_ = failDedupTx(ctx, tx, dedupKey, "route miss")
		return AdmitResult{Reason: RejectRouteMiss}, tx.Commit()
	}
	if err != nil {
		return AdmitResult{}, err
	}
	if task.CreatedBy != in.RequestedBy {
		_ = failDedupTx(ctx, tx, dedupKey, "creator mismatch")
		return AdmitResult{Reason: RejectCreatorMismatch, Task: task}, tx.Commit()
	}
	if task.CodexThreadID == "" {
		_ = failDedupTx(ctx, tx, dedupKey, "missing codex thread id")
		return AdmitResult{Reason: RejectMissingThread, Task: task}, tx.Commit()
	}
	if !taskCanStart(task.Status) {
		_ = failDedupTx(ctx, tx, dedupKey, "active run")
		return AdmitResult{Reason: RejectActiveRun, Task: task}, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO runs(id,task_id,kind,status,prompt,codex_thread_id,started_at)
VALUES(?,?,?,?,?,?,?)`, in.RunID, in.TaskID, "resume", "queued", in.Prompt, task.CodexThreadID, formatTime(now)); err != nil {
		if isUniqueConstraint(err) {
			_ = failDedupTx(ctx, tx, dedupKey, "active run")
			return AdmitResult{Reason: RejectActiveRun, Task: task}, tx.Commit()
		}
		return AdmitResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status='queued',updated_at=? WHERE id=?`, formatTime(now), in.TaskID); err != nil {
		return AdmitResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE event_dedup SET task_id=?,run_id=? WHERE dedup_key=?`, in.TaskID, in.RunID, dedupKey); err != nil {
		return AdmitResult{}, err
	}
	task, err = getTaskTx(ctx, tx, in.TaskID)
	if err != nil {
		return AdmitResult{}, err
	}
	run, err := getRunTx(ctx, tx, in.RunID)
	if err != nil {
		return AdmitResult{}, err
	}
	return AdmitResult{Task: task, Run: run}, tx.Commit()
}

func (s *Store) StartRun(ctx context.Context, in StartRunInput) (Task, Run, error) {
	now := normalizeTime(in.Now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, Run{}, err
	}
	defer rollback(tx)
	var taskID string
	if err := tx.QueryRowContext(ctx, `SELECT task_id FROM runs WHERE id=?`, in.RunID).Scan(&taskID); err != nil {
		return Task{}, Run{}, err
	}
	result, err := tx.ExecContext(ctx, `
UPDATE runs
SET status=CASE WHEN status='queued' THEN 'running' ELSE status END,
	codex_thread_id=COALESCE(NULLIF(?,''),codex_thread_id),
	codex_turn_id=COALESCE(NULLIF(?,''),codex_turn_id)
WHERE id=? AND status IN ('queued','running')`, in.ThreadID, in.TurnID, in.RunID)
	if err != nil {
		return Task{}, Run{}, err
	}
	if changed, err := result.RowsAffected(); err != nil {
		return Task{}, Run{}, err
	} else if changed != 1 {
		return Task{}, Run{}, ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE tasks
SET status=CASE WHEN status='queued' THEN 'running' ELSE status END,
	codex_thread_id=COALESCE(NULLIF(?,''),codex_thread_id),updated_at=?
WHERE id=? AND status IN ('queued','running')`, in.ThreadID, formatTime(now), taskID); err != nil {
		return Task{}, Run{}, err
	}
	task, err := getTaskTx(ctx, tx, taskID)
	if err != nil {
		return Task{}, Run{}, err
	}
	run, err := getRunTx(ctx, tx, in.RunID)
	if err != nil {
		return Task{}, Run{}, err
	}
	return task, run, tx.Commit()
}

func (s *Store) FinishRun(ctx context.Context, dedupKey string, in FinishRunInput) error {
	if !terminalRunStatus(in.Status) {
		return fmt.Errorf("invalid terminal run status %q", in.Status)
	}
	now := normalizeTime(in.FinishedAt)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)
	var taskID string
	if err := tx.QueryRowContext(ctx, `SELECT task_id FROM runs WHERE id=?`, in.RunID).Scan(&taskID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE runs SET status=?,codex_thread_id=COALESCE(NULLIF(?,''),codex_thread_id),codex_turn_id=COALESCE(NULLIF(?,''),codex_turn_id),exit_code=?,finished_at=?,final_text=?
WHERE id=?`, in.Status, in.ThreadID, in.TurnID, in.ExitCode, formatTime(now), nullString(in.FinalText), in.RunID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE tasks SET status=?,codex_thread_id=COALESCE(NULLIF(?,''),codex_thread_id),updated_at=? WHERE id=?`, in.Status, in.ThreadID, formatTime(now), taskID); err != nil {
		return err
	}
	if dedupKey != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE event_dedup SET state='completed',task_id=?,run_id=?,completed_at=?,last_error=NULL WHERE dedup_key=?`, taskID, in.RunID, formatTime(now), dedupKey); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) RefreshUsers(ctx context.Context, allowedOpenIDs []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if _, err := tx.ExecContext(ctx, `UPDATE users SET enabled=0`); err != nil {
		return err
	}
	for _, openID := range allowedOpenIDs {
		if openID == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO users(feishu_open_id,role,enabled) VALUES(?, 'owner', 1)
ON CONFLICT(feishu_open_id) DO UPDATE SET role='owner',enabled=1`, openID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) UserEnabled(ctx context.Context, openID string) (bool, error) {
	var enabled int
	err := s.db.QueryRowContext(ctx, `SELECT enabled FROM users WHERE feishu_open_id=?`, openID).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return enabled == 1, nil
}

func (s *Store) InsertMessageRoute(ctx context.Context, messageID, taskID, routeType string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO message_routes(feishu_message_id,task_id,route_type,created_at)
VALUES(?,?,?,?)`, messageID, taskID, routeType, formatTime(time.Now().UTC()))
	return err
}

func (s *Store) SetTaskRootMessageID(ctx context.Context, taskID, messageID string, now time.Time) error {
	if messageID == "" {
		return errors.New("root message id is required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE tasks SET root_message_id=?,updated_at=? WHERE id=?`, messageID, formatTime(now), taskID)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil {
		return err
	} else if changed != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ResolveMessageRoute(ctx context.Context, messageID string) (Task, error) {
	var taskID string
	err := s.db.QueryRowContext(ctx, `SELECT task_id FROM message_routes WHERE feishu_message_id=?`, messageID).Scan(&taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrRouteMiss
	}
	if err != nil {
		return Task{}, err
	}
	return s.getTask(ctx, taskID)
}

func (s *Store) ListTasks(ctx context.Context, limit int) ([]Task, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskColumns+` FROM tasks ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	tasks := make([]Task, 0)
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

// HasActiveTask reports whether a restart would interrupt an admitted bridge
// task. It intentionally checks task status rather than local goroutine state
// so restart safety remains correct across app-server event timing.
func (s *Store) HasActiveTask(ctx context.Context) (bool, error) {
	var active bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE status IN ('queued','running'))`).Scan(&active)
	return active, err
}

func (s *Store) GetTask(ctx context.Context, taskID string) (Task, []Run, error) {
	task, err := s.getTask(ctx, taskID)
	if err != nil {
		return Task{}, nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+runColumns+` FROM runs WHERE task_id=? ORDER BY started_at DESC`, taskID)
	if err != nil {
		return Task{}, nil, err
	}
	defer func() { _ = rows.Close() }()
	runs := make([]Run, 0)
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return Task{}, nil, err
		}
		runs = append(runs, run)
	}
	return task, runs, rows.Err()
}

func (s *Store) RecoverRunning(ctx context.Context, now time.Time) error {
	now = normalizeTime(now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if _, err := tx.ExecContext(ctx, `
UPDATE runs SET status='failed',exit_code=-1,finished_at=?,final_text='interrupted after bridge restart'
WHERE status IN ('queued','running')`, formatTime(now)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE tasks SET status='failed',updated_at=?
WHERE status IN ('queued','running')`, formatTime(now)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE event_dedup SET state='failed',completed_at=?,last_error='interrupted after bridge restart' WHERE state='processing'`, formatTime(now)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) getTask(ctx context.Context, taskID string) (Task, error) {
	task, err := getTaskQuery(ctx, s.db, taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrNotFound
	}
	return task, err
}

func insertDedup(ctx context.Context, tx *sql.Tx, dedupKey, source string, now time.Time) (bool, error) {
	if dedupKey == "" {
		return false, errors.New("dedup key is required")
	}
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO event_dedup(dedup_key,received_at,source,state) VALUES(?,?,?,'processing')`, dedupKey, formatTime(now), source)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

func failDedupTx(ctx context.Context, tx *sql.Tx, dedupKey, message string) error {
	_, err := tx.ExecContext(ctx, `UPDATE event_dedup SET state='failed',completed_at=?,last_error=? WHERE dedup_key=?`, formatTime(time.Now().UTC()), message, dedupKey)
	return err
}

const taskColumns = `id,codex_thread_id,status,project_alias,cwd,created_by,chat_id,root_message_id`
const runColumns = `id,task_id,kind,status,prompt,codex_thread_id,codex_turn_id,exit_code,final_text`

type taskScanner interface {
	Scan(dest ...any) error
}

type runScanner interface {
	Scan(dest ...any) error
}

func getTaskTx(ctx context.Context, tx *sql.Tx, taskID string) (Task, error) {
	return getTaskQuery(ctx, tx, taskID)
}

func getTaskQuery(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, taskID string) (Task, error) {
	return scanTask(q.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=?`, taskID))
}

func getRunTx(ctx context.Context, tx *sql.Tx, runID string) (Run, error) {
	return scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id=?`, runID))
}

func scanTask(scanner taskScanner) (Task, error) {
	var task Task
	var thread sql.NullString
	if err := scanner.Scan(&task.ID, &thread, &task.Status, &task.ProjectAlias, &task.CWD, &task.CreatedBy, &task.ChatID, &task.RootMessageID); err != nil {
		return Task{}, err
	}
	if thread.Valid {
		task.CodexThreadID = thread.String
	}
	return task, nil
}

func scanRun(scanner runScanner) (Run, error) {
	var run Run
	var thread, turn, finalText sql.NullString
	if err := scanner.Scan(&run.ID, &run.TaskID, &run.Kind, &run.Status, &run.Prompt, &thread, &turn, &run.ExitCode, &finalText); err != nil {
		return Run{}, err
	}
	if thread.Valid {
		run.CodexThreadID = thread.String
	}
	if turn.Valid {
		run.CodexTurnID = turn.String
	}
	if finalText.Valid {
		run.FinalText = finalText.String
	}
	return run, nil
}

func taskCanStart(status string) bool {
	return status == "idle" || status == "succeeded" || status == "failed" || status == "canceled"
}

func terminalRunStatus(status string) bool {
	return status == "succeeded" || status == "failed" || status == "canceled"
}

func rollback(tx *sql.Tx) {
	_ = tx.Rollback()
}

func normalizeTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now().UTC()
	}
	return value.UTC()
}

func formatTime(value time.Time) string {
	return normalizeTime(value).Format(time.RFC3339Nano)
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func isUniqueConstraint(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "unique")
}
