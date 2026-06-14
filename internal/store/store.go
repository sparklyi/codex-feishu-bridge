package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sparklyi/codex-feishu-bridge/internal/contracts"
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
	RejectMissingSession  RejectionReason = "missing_session"
)

type CreateTaskInput struct {
	TaskID                         string
	RunID                          string
	ProjectAlias                   string
	CWD                            string
	CreatedBy                      string
	ChatID                         string
	Prompt                         string
	EffectiveCodexCommand          string
	EffectiveSandbox               string
	EffectiveModel                 string
	EffectiveApproval              string
	EffectiveApprovalFlagSupported bool
	EffectiveExtraArgs             []string
	Now                            time.Time
}

type ResumeRunInput struct {
	RunID       string
	TaskID      string
	RequestedBy string
	Prompt      string
	Now         time.Time
}

type AdmitResult struct {
	Replay bool
	Reason RejectionReason
	Task   Task
	Run    Run
}

type Task struct {
	ID                             string
	CodexSessionID                 string
	Status                         string
	ProjectAlias                   string
	CWD                            string
	CreatedBy                      string
	ChatID                         string
	RootMessageID                  string
	EffectiveCodexCommand          string
	EffectiveSandbox               string
	EffectiveModel                 string
	EffectiveApproval              string
	EffectiveApprovalFlagSupported bool
	EffectiveExtraArgs             []string
}

type Run struct {
	ID             string
	TaskID         string
	Kind           string
	Status         string
	Prompt         string
	CodexSessionID string
	ExitCode       int
	LogPath        string
	FinalText      string
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
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return err
	}
	for i, migration := range migrations {
		version := i + 1
		var existing int
		err := s.db.QueryRowContext(ctx, `SELECT version FROM schema_migrations WHERE version = ?`, version).Scan(&existing)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migration); err != nil {
			_ = tx.Rollback()
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)`, version, formatTime(time.Now().UTC())); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
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
	argsJSON, err := json.Marshal(in.EffectiveExtraArgs)
	if err != nil {
		return AdmitResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO tasks(id,status,project_alias,cwd,created_by,chat_id,effective_codex_command,effective_sandbox,effective_model,effective_approval,effective_approval_flag_supported,effective_extra_args_json,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		in.TaskID, "running", in.ProjectAlias, in.CWD, in.CreatedBy, in.ChatID, in.EffectiveCodexCommand, in.EffectiveSandbox, nullString(in.EffectiveModel), in.EffectiveApproval, boolInt(in.EffectiveApprovalFlagSupported), string(argsJSON), formatTime(now), formatTime(now)); err != nil {
		return AdmitResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO runs(id,task_id,kind,status,prompt,started_at)
VALUES(?,?,?,?,?,?)`,
		in.RunID, in.TaskID, "exec", "running", in.Prompt, formatTime(now)); err != nil {
		return AdmitResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE event_dedup SET task_id=?, run_id=? WHERE dedup_key=?`, in.TaskID, in.RunID, dedupKey); err != nil {
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
	if task.CodexSessionID == "" {
		_ = failDedupTx(ctx, tx, dedupKey, "missing codex session id")
		return AdmitResult{Reason: RejectMissingSession, Task: task}, tx.Commit()
	}
	if task.Status == "running" {
		_ = failDedupTx(ctx, tx, dedupKey, "active run")
		return AdmitResult{Reason: RejectActiveRun, Task: task}, tx.Commit()
	}
	if task.Status != "succeeded" && task.Status != "failed" {
		_ = failDedupTx(ctx, tx, dedupKey, "status rejected")
		return AdmitResult{Reason: RejectStatus, Task: task}, tx.Commit()
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO runs(id,task_id,kind,status,prompt,codex_session_id,started_at)
VALUES(?,?,?,?,?,?,?)`,
		in.RunID, in.TaskID, "resume", "running", in.Prompt, task.CodexSessionID, formatTime(now))
	if err != nil {
		if isUniqueConstraint(err) {
			_ = failDedupTx(ctx, tx, dedupKey, "active run")
			return AdmitResult{Reason: RejectActiveRun, Task: task}, tx.Commit()
		}
		return AdmitResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status='running', updated_at=? WHERE id=?`, formatTime(now), in.TaskID); err != nil {
		return AdmitResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE event_dedup SET task_id=?, run_id=? WHERE dedup_key=?`, in.TaskID, in.RunID, dedupKey); err != nil {
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

func (s *Store) RecordRunSession(ctx context.Context, runID, threadID string) error {
	if threadID == "" {
		return errors.New("thread id is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)
	var taskID string
	var existing sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT task_id, codex_session_id FROM runs WHERE id=?`, runID).Scan(&taskID, &existing); err != nil {
		return err
	}
	if existing.Valid && existing.String != "" && existing.String != threadID {
		return fmt.Errorf("run %s already recorded session %s", runID, existing.String)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET codex_session_id=? WHERE id=?`, threadID, runID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET codex_session_id=?, updated_at=? WHERE id=?`, threadID, formatTime(time.Now().UTC()), taskID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) FinishRun(ctx context.Context, dedupKey, runID string, result contracts.RunResult, status string) error {
	now := normalizeTime(result.FinishedAt)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)
	var taskID string
	if err := tx.QueryRowContext(ctx, `SELECT task_id FROM runs WHERE id=?`, runID).Scan(&taskID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE runs SET status=?, codex_session_id=COALESCE(NULLIF(?, ''), codex_session_id), exit_code=?, finished_at=?, log_path=?, final_text=? WHERE id=?`,
		status, result.CodexSessionID, result.ExitCode, formatTime(now), result.LogPath, result.FinalText, runID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE tasks SET status=?, codex_session_id=COALESCE(NULLIF(?, ''), codex_session_id), updated_at=? WHERE id=?`,
		status, result.CodexSessionID, formatTime(now), taskID); err != nil {
		return err
	}
	if dedupKey != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE event_dedup SET state='completed', task_id=?, run_id=?, completed_at=?, last_error=NULL WHERE dedup_key=?`, taskID, runID, formatTime(now), dedupKey); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) FailDedup(ctx context.Context, dedupKey string, err error) error {
	if dedupKey == "" {
		return nil
	}
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	_, execErr := s.db.ExecContext(ctx, `UPDATE event_dedup SET state='failed', completed_at=?, last_error=? WHERE dedup_key=?`, formatTime(time.Now().UTC()), msg, dedupKey)
	return execErr
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
INSERT INTO users(feishu_open_id, role, enabled) VALUES(?, 'owner', 1)
ON CONFLICT(feishu_open_id) DO UPDATE SET role='owner', enabled=1`, openID); err != nil {
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
INSERT INTO message_routes(feishu_message_id, task_id, route_type, created_at)
VALUES(?,?,?,?)`, messageID, taskID, routeType, formatTime(time.Now().UTC()))
	return err
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

func (s *Store) ListTasks(ctx context.Context, limit int) ([]Task, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskColumns+` FROM tasks ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []Task
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

func (s *Store) GetTask(ctx context.Context, taskID string) (Task, []Run, error) {
	task, err := s.getTask(ctx, taskID)
	if err != nil {
		return Task{}, nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,task_id,kind,status,prompt,codex_session_id,exit_code,log_path,final_text FROM runs WHERE task_id=? ORDER BY started_at DESC`, taskID)
	if err != nil {
		return Task{}, nil, err
	}
	defer rows.Close()
	var runs []Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return Task{}, nil, err
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return Task{}, nil, err
	}
	return task, runs, nil
}

func (s *Store) RecoverRunning(ctx context.Context, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)
	rows, err := tx.QueryContext(ctx, `SELECT id, task_id FROM runs WHERE status='running'`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type runningRun struct {
		runID  string
		taskID string
	}
	var running []runningRun
	for rows.Next() {
		var rr runningRun
		if err := rows.Scan(&rr.runID, &rr.taskID); err != nil {
			return err
		}
		running = append(running, rr)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, rr := range running {
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET status='failed', exit_code=-1, finished_at=?, final_text=? WHERE id=?`, formatTime(now), "failed after daemon restart", rr.runID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status='failed', updated_at=? WHERE id=? AND status='running'`, formatTime(now), rr.taskID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE event_dedup SET state='failed', completed_at=?, last_error='failed after daemon restart' WHERE state='processing'`, formatTime(now)); err != nil {
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
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO event_dedup(dedup_key, received_at, source, state) VALUES(?,?,?,'processing')`, dedupKey, formatTime(now), source)
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	return rows == 1, err
}

func failDedupTx(ctx context.Context, tx *sql.Tx, dedupKey, message string) error {
	_, err := tx.ExecContext(ctx, `UPDATE event_dedup SET state='failed', completed_at=?, last_error=? WHERE dedup_key=?`, formatTime(time.Now().UTC()), message, dedupKey)
	return err
}

const taskColumns = `id,codex_session_id,status,project_alias,cwd,created_by,chat_id,root_message_id,effective_codex_command,effective_sandbox,effective_model,effective_approval,effective_approval_flag_supported,effective_extra_args_json`

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
	return scanRun(tx.QueryRowContext(ctx, `SELECT id,task_id,kind,status,prompt,codex_session_id,exit_code,log_path,final_text FROM runs WHERE id=?`, runID))
}

func scanTask(scanner taskScanner) (Task, error) {
	var task Task
	var session, model sql.NullString
	var extraJSON string
	var approvalFlag int
	if err := scanner.Scan(&task.ID, &session, &task.Status, &task.ProjectAlias, &task.CWD, &task.CreatedBy, &task.ChatID, &task.RootMessageID, &task.EffectiveCodexCommand, &task.EffectiveSandbox, &model, &task.EffectiveApproval, &approvalFlag, &extraJSON); err != nil {
		return Task{}, err
	}
	if session.Valid {
		task.CodexSessionID = session.String
	}
	if model.Valid {
		task.EffectiveModel = model.String
	}
	task.EffectiveApprovalFlagSupported = approvalFlag == 1
	if extraJSON == "" {
		extraJSON = "[]"
	}
	if err := json.Unmarshal([]byte(extraJSON), &task.EffectiveExtraArgs); err != nil {
		return Task{}, err
	}
	return task, nil
}

func scanRun(scanner runScanner) (Run, error) {
	var run Run
	var session, logPath, finalText sql.NullString
	if err := scanner.Scan(&run.ID, &run.TaskID, &run.Kind, &run.Status, &run.Prompt, &session, &run.ExitCode, &logPath, &finalText); err != nil {
		return Run{}, err
	}
	if session.Valid {
		run.CodexSessionID = session.String
	}
	if logPath.Valid {
		run.LogPath = logPath.String
	}
	if finalText.Valid {
		run.FinalText = finalText.String
	}
	return run, nil
}

func rollback(tx *sql.Tx) {
	_ = tx.Rollback()
}

func normalizeTime(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now().UTC()
	}
	return t.UTC()
}

func formatTime(t time.Time) string {
	return normalizeTime(t).Format(time.RFC3339Nano)
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func isUniqueConstraint(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "unique")
}
