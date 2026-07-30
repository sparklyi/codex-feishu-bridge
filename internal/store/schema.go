package store

const migrationVersion = 3

// schema is intentionally installed only for a fresh state database. This
// bridge does not migrate old state because its task and permission model is a
// breaking operational boundary.
const schema = `
CREATE TABLE schema_migrations (
	version INTEGER PRIMARY KEY,
	applied_at TEXT NOT NULL
);

CREATE TABLE tasks (
	id TEXT PRIMARY KEY,
	codex_thread_id TEXT,
	status TEXT NOT NULL CHECK (status IN ('idle','queued','running','succeeded','failed','canceled')),
	project_alias TEXT NOT NULL,
	cwd TEXT NOT NULL,
	created_by TEXT NOT NULL,
	chat_id TEXT NOT NULL,
	root_message_id TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE runs (
	id TEXT PRIMARY KEY,
	task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	kind TEXT NOT NULL CHECK (kind IN ('new','resume')),
	status TEXT NOT NULL CHECK (status IN ('queued','running','succeeded','failed','canceled')),
	prompt TEXT NOT NULL,
	codex_thread_id TEXT,
	codex_turn_id TEXT,
	exit_code INTEGER NOT NULL DEFAULT 0,
	started_at TEXT NOT NULL,
	finished_at TEXT,
	final_text TEXT
);

CREATE TABLE message_routes (
	feishu_message_id TEXT PRIMARY KEY,
	task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	route_type TEXT NOT NULL,
	created_at TEXT NOT NULL
);

CREATE TABLE event_dedup (
	dedup_key TEXT PRIMARY KEY,
	received_at TEXT NOT NULL,
	source TEXT NOT NULL CHECK (source IN ('message','card_callback')),
	state TEXT NOT NULL CHECK (state IN ('processing','completed','failed')),
	task_id TEXT REFERENCES tasks(id) ON DELETE SET NULL,
	run_id TEXT REFERENCES runs(id) ON DELETE SET NULL,
	completed_at TEXT,
	last_error TEXT
);

CREATE TABLE users (
	feishu_open_id TEXT PRIMARY KEY,
	role TEXT NOT NULL,
	enabled INTEGER NOT NULL CHECK (enabled IN (0,1))
);

CREATE TABLE card_stream_sequences (
	feishu_message_id TEXT PRIMARY KEY,
	last_sequence INTEGER NOT NULL CHECK (last_sequence BETWEEN 1 AND 2147483647)
);

CREATE INDEX idx_tasks_codex_thread_id ON tasks(codex_thread_id);
CREATE INDEX idx_runs_task_id ON runs(task_id);
CREATE INDEX idx_runs_status ON runs(status);
CREATE INDEX idx_message_routes_task_id ON message_routes(task_id);
CREATE UNIQUE INDEX runs_one_active_per_task
	ON runs(task_id) WHERE status IN ('queued','running');
`
