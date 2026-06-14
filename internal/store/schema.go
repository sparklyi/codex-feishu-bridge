package store

const migrationVersion = 1

var migrations = []string{
	`
CREATE TABLE IF NOT EXISTS schema_migrations (
	version INTEGER PRIMARY KEY,
	applied_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS tasks (
	id TEXT PRIMARY KEY,
	codex_session_id TEXT,
	status TEXT NOT NULL CHECK (status IN ('queued','running','succeeded','failed')),
	project_alias TEXT NOT NULL,
	cwd TEXT NOT NULL,
	created_by TEXT NOT NULL,
	chat_id TEXT NOT NULL,
	root_message_id TEXT NOT NULL DEFAULT '',
	effective_codex_command TEXT NOT NULL,
	effective_sandbox TEXT NOT NULL,
	effective_model TEXT,
	effective_approval TEXT NOT NULL,
	effective_approval_flag_supported INTEGER NOT NULL CHECK (effective_approval_flag_supported IN (0,1)),
	effective_extra_args_json TEXT NOT NULL DEFAULT '[]',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS message_routes (
	feishu_message_id TEXT PRIMARY KEY,
	task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	route_type TEXT NOT NULL CHECK (route_type IN ('start_card','result_card','manual_reply')),
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS runs (
	id TEXT PRIMARY KEY,
	task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	kind TEXT NOT NULL CHECK (kind IN ('exec','resume')),
	status TEXT NOT NULL CHECK (status IN ('running','succeeded','failed')),
	prompt TEXT NOT NULL,
	codex_session_id TEXT,
	exit_code INTEGER NOT NULL DEFAULT 0,
	started_at TEXT NOT NULL,
	finished_at TEXT,
	log_path TEXT,
	final_text TEXT
);

CREATE TABLE IF NOT EXISTS event_dedup (
	dedup_key TEXT PRIMARY KEY,
	received_at TEXT NOT NULL,
	source TEXT NOT NULL CHECK (source IN ('message','card_callback')),
	state TEXT NOT NULL CHECK (state IN ('processing','completed','failed')),
	task_id TEXT REFERENCES tasks(id) ON DELETE SET NULL,
	run_id TEXT REFERENCES runs(id) ON DELETE SET NULL,
	completed_at TEXT,
	last_error TEXT
);

CREATE TABLE IF NOT EXISTS users (
	feishu_open_id TEXT PRIMARY KEY,
	role TEXT NOT NULL,
	enabled INTEGER NOT NULL CHECK (enabled IN (0,1))
);

CREATE INDEX IF NOT EXISTS idx_tasks_codex_session_id ON tasks(codex_session_id);
CREATE INDEX IF NOT EXISTS idx_runs_task_id ON runs(task_id);
CREATE INDEX IF NOT EXISTS idx_runs_status ON runs(status);
CREATE INDEX IF NOT EXISTS idx_message_routes_task_id ON message_routes(task_id);
CREATE UNIQUE INDEX IF NOT EXISTS runs_one_active_per_task ON runs(task_id) WHERE status = 'running';
`,
}
