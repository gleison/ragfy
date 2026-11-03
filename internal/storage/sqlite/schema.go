package sqlite

const schema = `PRAGMA journal_mode=WAL;
CREATE TABLE IF NOT EXISTS docs (
 id INTEGER PRIMARY KEY, source TEXT NOT NULL, source_id TEXT NOT NULL,
 ts TEXT, title TEXT, content TEXT NOT NULL, metadata TEXT, kind TEXT, area TEXT,
 embedding BLOB, UNIQUE(source,source_id));
CREATE TABLE IF NOT EXISTS digested_sessions (
 session_id TEXT PRIMARY KEY, transcript_hash TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS app_metadata (
 key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS docs_source ON docs(source);
CREATE INDEX IF NOT EXISTS docs_kind ON docs(kind);`
