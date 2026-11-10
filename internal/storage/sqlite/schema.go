package sqlite

const schema = `PRAGMA journal_mode=WAL;
CREATE TABLE IF NOT EXISTS docs (
 id INTEGER PRIMARY KEY, source TEXT NOT NULL, source_id TEXT NOT NULL,
 ts TEXT, title TEXT, content TEXT NOT NULL, metadata TEXT, kind TEXT, area TEXT,
 scope TEXT NOT NULL, project TEXT NOT NULL, embedding BLOB,
 status TEXT NOT NULL DEFAULT 'active', superseded_by TEXT NOT NULL DEFAULT '',
 superseded_at TEXT NOT NULL DEFAULT '', consolidation_reason TEXT NOT NULL DEFAULT '', origin_id TEXT NOT NULL DEFAULT '',
 UNIQUE(source,source_id));
CREATE TABLE IF NOT EXISTS knowledge_sources (
 id TEXT PRIMARY KEY, kind TEXT NOT NULL, source_key TEXT NOT NULL, provider TEXT NOT NULL DEFAULT '',
 project TEXT NOT NULL DEFAULT '', title TEXT NOT NULL DEFAULT '', content TEXT NOT NULL,
 content_hash TEXT NOT NULL, created_at TEXT NOT NULL,
 UNIQUE(kind,source_key,content_hash,provider,project));
CREATE TABLE IF NOT EXISTS digested_sessions (
 session_id TEXT PRIMARY KEY, transcript_hash TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS recall_events (
 id TEXT PRIMARY KEY, session_id TEXT NOT NULL DEFAULT '', source TEXT NOT NULL,
 project TEXT NOT NULL DEFAULT '', prompt TEXT NOT NULL, created_at TEXT NOT NULL,
 score_cutoff REAL NOT NULL DEFAULT 0, lexical_cutoff REAL NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS recall_results (
 recall_id TEXT NOT NULL, rank INTEGER NOT NULL, source_id TEXT NOT NULL,
 title TEXT NOT NULL DEFAULT '', content TEXT NOT NULL, kind TEXT NOT NULL DEFAULT '',
 scope TEXT NOT NULL, project TEXT NOT NULL DEFAULT '', score REAL NOT NULL,
 semantic_score REAL NOT NULL, lexical_score REAL NOT NULL, project_boost REAL NOT NULL,
 selected_rank INTEGER NOT NULL DEFAULT 0, status TEXT NOT NULL DEFAULT 'selected',
 feedback TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(recall_id,rank));
CREATE TABLE IF NOT EXISTS app_metadata (
 key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS docs_source ON docs(source);
CREATE INDEX IF NOT EXISTS docs_kind ON docs(kind);
CREATE INDEX IF NOT EXISTS docs_scope_project ON docs(scope,project);
CREATE INDEX IF NOT EXISTS recall_events_session ON recall_events(session_id,created_at);`
