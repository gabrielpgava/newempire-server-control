CREATE TABLE users (
 id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE COLLATE NOCASE,
 password_hash TEXT NOT NULL, role TEXT NOT NULL CHECK(role IN ('owner','admin','operator','viewer')),
 totp BLOB, created_at INTEGER NOT NULL
);
CREATE TABLE sessions (
 token_hash TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 csrf TEXT NOT NULL, expires_at INTEGER NOT NULL
);
CREATE INDEX sessions_expiry ON sessions(expires_at);
CREATE TABLE instances (
 id TEXT PRIMARY KEY, name TEXT NOT NULL, root TEXT NOT NULL UNIQUE,
 port INTEGER NOT NULL UNIQUE, rcon_port INTEGER NOT NULL UNIQUE,
 data TEXT NOT NULL CHECK(json_valid(data)), secret BLOB NOT NULL
);
CREATE TABLE instance_permissions (
 user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 instance_id TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
 role TEXT NOT NULL CHECK(role IN ('operator','viewer')),
 PRIMARY KEY(user_id,instance_id)
);
CREATE TRIGGER instance_collision BEFORE INSERT ON instances
WHEN EXISTS(SELECT 1 FROM instances WHERE
 port=NEW.rcon_port OR rcon_port=NEW.port OR
 instr(NEW.root,root||'/')=1 OR instr(root,NEW.root||'/')=1)
BEGIN SELECT RAISE(ABORT,'instance ports or directories overlap'); END;
CREATE TABLE jobs (
 id TEXT PRIMARY KEY, instance_id TEXT NOT NULL DEFAULT '', kind TEXT NOT NULL,
 payload TEXT NOT NULL CHECK(json_valid(payload)),
 state TEXT NOT NULL CHECK(state IN ('queued','running','succeeded','failed','cancelled','interrupted')),
 progress INTEGER NOT NULL DEFAULT 0 CHECK(progress BETWEEN 0 AND 100),
 step TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '', result TEXT NOT NULL DEFAULT '{}',
 cancel_requested INTEGER NOT NULL DEFAULT 0, cancellable INTEGER NOT NULL DEFAULT 1,
 priority INTEGER NOT NULL DEFAULT 0, available_at INTEGER NOT NULL DEFAULT 0,
 attempts INTEGER NOT NULL DEFAULT 0, idempotency_key TEXT UNIQUE,
 created_at INTEGER NOT NULL, started_at INTEGER, finished_at INTEGER
);
CREATE INDEX jobs_claim ON jobs(state,priority,created_at);
CREATE TABLE job_steps (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, job_id TEXT NOT NULL REFERENCES jobs(id),
 time INTEGER NOT NULL, progress INTEGER NOT NULL, message TEXT NOT NULL
);
CREATE TABLE audit (
 id INTEGER PRIMARY KEY AUTOINCREMENT, time INTEGER NOT NULL, user_id TEXT NOT NULL,
 action TEXT NOT NULL, instance_id TEXT NOT NULL DEFAULT '', detail TEXT NOT NULL
);
CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL CHECK(json_valid(value)));
CREATE TABLE secrets (key TEXT PRIMARY KEY, value BLOB NOT NULL);
CREATE TABLE schedules (
 id TEXT PRIMARY KEY, instance_id TEXT NOT NULL REFERENCES instances(id),
 kind TEXT NOT NULL, data TEXT NOT NULL CHECK(json_valid(data)),
 enabled INTEGER NOT NULL, next_run INTEGER NOT NULL, last_job_id TEXT NOT NULL DEFAULT ''
);
CREATE TABLE metrics (
 instance_id TEXT NOT NULL REFERENCES instances(id), time INTEGER NOT NULL,
 state TEXT NOT NULL, memory_bytes INTEGER NOT NULL, cpu_seconds REAL NOT NULL,
 PRIMARY KEY(instance_id,time)
);
CREATE TABLE notifications (
 id TEXT PRIMARY KEY, time INTEGER NOT NULL, level TEXT NOT NULL,
 message TEXT NOT NULL, delivered INTEGER NOT NULL DEFAULT 0, attempts INTEGER NOT NULL DEFAULT 0,
 next_attempt INTEGER NOT NULL, last_error TEXT NOT NULL DEFAULT ''
);
