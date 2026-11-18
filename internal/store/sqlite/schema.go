package sqlite

// migrations are applied in order, once each (sqldb.Migrate). Statements are
// idempotent so two processes migrating at once cannot trip each other.
// Records are JSON documents; the columns beside them are what is queried.
var migrations = map[int]string{
	1: `
CREATE TABLE IF NOT EXISTS runs (
	seq        INTEGER PRIMARY KEY AUTOINCREMENT,
	id         TEXT    NOT NULL UNIQUE,
	flow       TEXT    NOT NULL,
	idem_key   TEXT    NOT NULL DEFAULT '',
	status     TEXT    NOT NULL,
	grp        TEXT    NOT NULL DEFAULT '',
	admitted   INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER,
	doc        TEXT    NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS runs_idem ON runs (flow, idem_key) WHERE idem_key <> '';
CREATE INDEX IF NOT EXISTS runs_group ON runs (grp, admitted, status);
CREATE INDEX IF NOT EXISTS runs_flow ON runs (flow, status);

CREATE TABLE IF NOT EXISTS steps (
	seq    INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id TEXT NOT NULL,
	id     TEXT NOT NULL,
	doc    TEXT NOT NULL,
	UNIQUE (run_id, id)
);

CREATE TABLE IF NOT EXISTS state_kv (
	flow  TEXT NOT NULL,
	key   TEXT NOT NULL,
	value TEXT NOT NULL,
	PRIMARY KEY (flow, key)
);

CREATE TABLE IF NOT EXISTS staged (
	run_id TEXT NOT NULL,
	key    TEXT NOT NULL,
	value  TEXT NOT NULL,
	PRIMARY KEY (run_id, key)
);

CREATE TABLE IF NOT EXISTS flow_versions (
	seq        INTEGER PRIMARY KEY AUTOINCREMENT,
	flow       TEXT    NOT NULL,
	hash       TEXT    NOT NULL,
	version    TEXT    NOT NULL,
	vseq       INTEGER NOT NULL,
	state      TEXT    NOT NULL,
	source     BLOB,
	doc        TEXT    NOT NULL,
	UNIQUE (flow, hash)
);

CREATE TABLE IF NOT EXISTS approvals (
	seq      INTEGER PRIMARY KEY AUTOINCREMENT,
	id       TEXT NOT NULL UNIQUE,
	run_id   TEXT NOT NULL,
	step_run TEXT NOT NULL,
	step_id  TEXT NOT NULL,
	status   TEXT NOT NULL,
	version  INTEGER NOT NULL,
	doc      TEXT NOT NULL,
	UNIQUE (run_id, step_run)
);

CREATE TABLE IF NOT EXISTS grants (
	profile    TEXT NOT NULL,
	capability TEXT NOT NULL,
	source     TEXT NOT NULL,
	created_at INTEGER,
	PRIMARY KEY (profile, capability)
);

CREATE TABLE IF NOT EXISTS receipts (
	seq    INTEGER PRIMARY KEY AUTOINCREMENT,
	id     TEXT NOT NULL UNIQUE,
	run_id TEXT NOT NULL,
	doc    TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS receipts_run ON receipts (run_id, seq);

CREATE TABLE IF NOT EXISTS outputs (
	seq        INTEGER PRIMARY KEY AUTOINCREMENT,
	id         TEXT NOT NULL UNIQUE,
	run_id     TEXT NOT NULL,
	kind       TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	doc        TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS outputs_created ON outputs (created_at);

CREATE TABLE IF NOT EXISTS triggers (
	id      TEXT PRIMARY KEY,
	flow    TEXT NOT NULL,
	idx     INTEGER NOT NULL,
	next_at INTEGER,
	doc     TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS triggers_flow ON triggers (flow, idx);

CREATE TABLE IF NOT EXISTS fires (
	seq     INTEGER PRIMARY KEY AUTOINCREMENT,
	flow    TEXT NOT NULL,
	trig TEXT NOT NULL,
	doc     TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS fires_trigger ON fires (trig, seq);

CREATE TABLE IF NOT EXISTS planned_fires (
	trig TEXT NOT NULL,
	key     TEXT NOT NULL,
	flow    TEXT NOT NULL,
	at      INTEGER NOT NULL,
	doc     TEXT NOT NULL,
	PRIMARY KEY (trig, key)
);

CREATE TABLE IF NOT EXISTS group_claims (
	run_id TEXT PRIMARY KEY,
	grp    TEXT NOT NULL,
	at     INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS group_claims_grp ON group_claims (grp);
`,
}
