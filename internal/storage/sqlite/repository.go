package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"ragfy/internal/knowledge"

	_ "modernc.org/sqlite"
)

type Repository struct {
	database *sql.DB
}

var _ knowledge.Repository = (*Repository)(nil)

func Open(config Config) (*Repository, error) {
	if err := os.MkdirAll(filepath.Dir(config.Path), 0700); err != nil {
		return nil, err
	}
	database, err := sql.Open("sqlite", config.Path)
	if err != nil {
		return nil, err
	}
	maximumConnections := config.MaxOpenConnections
	if maximumConnections == 0 {
		maximumConnections = 1
	}
	database.SetMaxOpenConns(maximumConnections)
	if _, err = database.Exec(schema); err != nil {
		database.Close()
		return nil, err
	}
	if err = migrateSchema(database); err != nil {
		database.Close()
		return nil, err
	}
	return &Repository{database: database}, nil
}

func (repository *Repository) Close() error {
	return repository.database.Close()
}

func (repository *Repository) EmbeddingModel(ctx context.Context) (string, error) {
	var model string
	err := repository.database.QueryRowContext(ctx, `SELECT value FROM app_metadata WHERE key='embedding_model'`).Scan(&model)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return model, err
}

func (repository *Repository) SetEmbeddingModel(ctx context.Context, model string) error {
	_, err := repository.database.ExecContext(ctx, `INSERT INTO app_metadata(key,value) VALUES('embedding_model',?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value`, model)
	return err
}

func (repository *Repository) Statistics(ctx context.Context) (knowledge.Statistics, error) {
	var statistics knowledge.Statistics
	err := repository.database.QueryRowContext(ctx, `SELECT
COUNT(*) FILTER (WHERE status='active'),
COUNT(*) FILTER (WHERE status='superseded'),
(SELECT COUNT(*) FROM digested_sessions),
(SELECT COUNT(*) FROM knowledge_sources),
COUNT(DISTINCT CASE WHEN scope='project' THEN project END)
FROM docs`).Scan(&statistics.ActiveRecords, &statistics.SupersededRecords, &statistics.DigestedSessions, &statistics.PreservedSources, &statistics.Projects)
	return statistics, err
}

func (repository *Repository) Sources(ctx context.Context) ([]knowledge.Source, error) {
	rows, err := repository.database.QueryContext(ctx, `SELECT id,kind,source_key,provider,project,title,content,content_hash,created_at FROM knowledge_sources ORDER BY created_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sources []knowledge.Source
	for rows.Next() {
		var source knowledge.Source
		if err := rows.Scan(&source.ID, &source.Kind, &source.Key, &source.Provider, &source.Project, &source.Title, &source.Content, &source.ContentHash, &source.CreatedAt); err != nil {
			return nil, err
		}
		sources = append(sources, source)
	}
	return sources, rows.Err()
}

func (repository *Repository) Documents(ctx context.Context) ([]knowledge.Document, error) {
	rows, err := repository.database.QueryContext(ctx, `SELECT source,source_id,ts,title,content,metadata,kind,area,scope,project,embedding,status,superseded_by,superseded_at,consolidation_reason,origin_id FROM docs ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var documents []knowledge.Document
	for rows.Next() {
		var document knowledge.Document
		var metadata string
		var encodedVector []byte
		if err := rows.Scan(&document.Source, &document.SourceID, &document.TS, &document.Title, &document.Content, &metadata, &document.Kind, &document.Area, &document.Scope, &document.Project, &encodedVector, &document.Status, &document.SupersededBy, &document.SupersededAt, &document.ConsolidationReason, &document.OriginID); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(metadata), &document.Metadata)
		document.Vector = decodeVector(encodedVector)
		documents = append(documents, document)
	}
	return documents, rows.Err()
}

func (repository *Repository) UpdateEmbedding(ctx context.Context, source, sourceID string, vector []float32) error {
	_, err := repository.database.ExecContext(ctx, `UPDATE docs SET embedding=? WHERE source=? AND source_id=?`, encodeVector(vector), source, sourceID)
	return err
}

func (repository *Repository) Upsert(ctx context.Context, document knowledge.Document) error {
	if !document.HasValidScope() {
		return fmt.Errorf("invalid knowledge scope %q for project %q", document.Scope, document.Project)
	}
	metadata, _ := json.Marshal(document.Metadata)
	var vector []byte
	if len(document.Vector) > 0 {
		vector = encodeVector(document.Vector)
	}
	if document.Status == "" {
		document.Status = knowledge.StatusActive
	}
	_, err := repository.database.ExecContext(ctx, upsertDocumentSQL,
		document.Source, document.SourceID, document.TS, document.Title, document.Content, string(metadata), document.Kind, document.Area, document.Scope, document.Project, vector,
		document.Status, document.SupersededBy, document.SupersededAt, document.ConsolidationReason, document.OriginID)
	return err
}

const upsertDocumentSQL = `INSERT INTO docs(source,source_id,ts,title,content,metadata,kind,area,scope,project,embedding,status,superseded_by,superseded_at,consolidation_reason,origin_id)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(source,source_id) DO UPDATE SET ts=excluded.ts,title=excluded.title,
content=excluded.content,metadata=excluded.metadata,kind=excluded.kind,area=excluded.area,scope=excluded.scope,project=excluded.project,embedding=excluded.embedding,
status=excluded.status,superseded_by=excluded.superseded_by,superseded_at=excluded.superseded_at,consolidation_reason=excluded.consolidation_reason,origin_id=excluded.origin_id`

func (repository *Repository) HasSession(ctx context.Context, sessionID, transcriptHash string) (bool, error) {
	var storedHash string
	err := repository.database.QueryRowContext(ctx, `SELECT transcript_hash FROM digested_sessions WHERE session_id=?`, sessionID).Scan(&storedHash)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return storedHash == transcriptHash, err
}

func (repository *Repository) HasSource(ctx context.Context, sourceID string) (bool, error) {
	var count int
	err := repository.database.QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge_sources WHERE id=?`, sourceID).Scan(&count)
	return count == 1, err
}

func (repository *Repository) CommitSession(ctx context.Context, commit knowledge.SessionCommit) error {
	transaction, err := repository.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	now := time.Now().UTC().Format(time.RFC3339)
	legacyIDs, err := documentIDs(ctx, transaction, `SELECT source_id FROM docs WHERE status='active' AND origin_id='' AND source='transcripts' AND json_extract(metadata,'$.session_id')=?`, commit.SessionID)
	if err != nil {
		return err
	}
	if err := retireDocuments(ctx, transaction, legacyIDs, firstDocumentID(commit.Documents), now); err != nil {
		return err
	}

	if err := commitSourceTransaction(ctx, transaction, knowledge.SourceCommit{Source: commit.Source, Documents: commit.Documents, Supersessions: commit.Supersessions}); err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx, `INSERT INTO digested_sessions(session_id,transcript_hash,created_at) VALUES(?,?,?)
ON CONFLICT(session_id) DO UPDATE SET transcript_hash=excluded.transcript_hash,created_at=excluded.created_at`, commit.SessionID, commit.TranscriptHash, now); err != nil {
		return err
	}
	return transaction.Commit()
}

func (repository *Repository) CommitSource(ctx context.Context, commit knowledge.SourceCommit) error {
	transaction, err := repository.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	if err := commitSourceTransaction(ctx, transaction, commit); err != nil {
		return err
	}
	return transaction.Commit()
}

func commitSourceTransaction(ctx context.Context, transaction *sql.Tx, commit knowledge.SourceCommit) error {
	if err := validateSource(commit.Source); err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx, `INSERT INTO knowledge_sources(id,kind,source_key,provider,project,title,content,content_hash,created_at)
VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, commit.Source.ID, commit.Source.Kind, commit.Source.Key, commit.Source.Provider, commit.Source.Project, commit.Source.Title, commit.Source.Content, commit.Source.ContentHash, commit.Source.CreatedAt); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	priorIDs, err := documentIDs(ctx, transaction, `SELECT source_id FROM docs WHERE status='active' AND origin_id IN (SELECT id FROM knowledge_sources WHERE kind=? AND source_key=? AND project=? AND id<>?)`, commit.Source.Kind, commit.Source.Key, commit.Source.Project, commit.Source.ID)
	if err != nil {
		return err
	}
	if err := retireDocuments(ctx, transaction, priorIDs, firstDocumentID(commit.Documents), now); err != nil {
		return err
	}
	for _, document := range commit.Documents {
		if !document.HasValidScope() {
			return fmt.Errorf("invalid knowledge scope %q for project %q", document.Scope, document.Project)
		}
		document.OriginID = commit.Source.ID
		metadata, _ := json.Marshal(document.Metadata)
		if document.Status == "" {
			document.Status = knowledge.StatusActive
		}
		if _, err := transaction.ExecContext(ctx, upsertDocumentSQL,
			document.Source, document.SourceID, document.TS, document.Title, document.Content, string(metadata), document.Kind, document.Area, document.Scope, document.Project, encodeVector(document.Vector),
			document.Status, document.SupersededBy, document.SupersededAt, document.ConsolidationReason, document.OriginID); err != nil {
			return err
		}
	}
	return applySupersessions(ctx, transaction, commit.Supersessions, now)
}

func firstDocumentID(documents []knowledge.Document) string {
	if len(documents) == 0 {
		return ""
	}
	return documents[0].SourceID
}

func documentIDs(ctx context.Context, transaction *sql.Tx, query string, arguments ...any) ([]string, error) {
	rows, err := transaction.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func retireDocuments(ctx context.Context, transaction *sql.Tx, ids []string, replacement, now string) error {
	for _, id := range ids {
		if _, err := transaction.ExecContext(ctx, `UPDATE docs SET status='active',superseded_by='',superseded_at='',consolidation_reason='' WHERE superseded_by=?`, id); err != nil {
			return err
		}
		if _, err := transaction.ExecContext(ctx, `UPDATE docs SET status='superseded',superseded_by=?,superseded_at=?,consolidation_reason='source updated' WHERE source_id=?`, replacement, now, id); err != nil {
			return err
		}
	}
	return nil
}

func validateSource(source knowledge.Source) error {
	if source.ID == "" || source.Kind == "" || source.Key == "" || source.ContentHash == "" || source.CreatedAt == "" {
		return errors.New("knowledge source is incomplete")
	}
	return nil
}

func applySupersessions(ctx context.Context, transaction *sql.Tx, supersessions []knowledge.Supersession, now string) error {
	for _, supersession := range supersessions {
		if supersession.LoserSourceID == supersession.WinnerSourceID {
			return fmt.Errorf("knowledge record %q cannot supersede itself", supersession.LoserSourceID)
		}
		var winnerCount int
		if err := transaction.QueryRowContext(ctx, `SELECT COUNT(*) FROM docs WHERE source_id=?`, supersession.WinnerSourceID).Scan(&winnerCount); err != nil {
			return err
		}
		if winnerCount != 1 {
			return fmt.Errorf("supersession winner %q not found", supersession.WinnerSourceID)
		}
		result, err := transaction.ExecContext(ctx, `UPDATE docs SET status='superseded',superseded_by=?,superseded_at=?,consolidation_reason=? WHERE source_id=?`, supersession.WinnerSourceID, now, supersession.Reason, supersession.LoserSourceID)
		if err != nil {
			return err
		}
		if affected, err := result.RowsAffected(); err != nil || affected != 1 {
			return fmt.Errorf("supersession loser %q not found", supersession.LoserSourceID)
		}
	}
	return nil
}

func (repository *Repository) SearchableDocuments(ctx context.Context) ([]knowledge.Document, error) {
	rows, err := repository.database.QueryContext(ctx, `SELECT source,source_id,ts,title,content,metadata,kind,area,scope,project,embedding,status,superseded_by,superseded_at,consolidation_reason,origin_id FROM docs WHERE embedding IS NOT NULL AND status='active'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	documents := make([]knowledge.Document, 0)
	for rows.Next() {
		var document knowledge.Document
		var metadata string
		var encodedVector []byte
		if err := rows.Scan(&document.Source, &document.SourceID, &document.TS, &document.Title, &document.Content, &metadata, &document.Kind, &document.Area, &document.Scope, &document.Project, &encodedVector, &document.Status, &document.SupersededBy, &document.SupersededAt, &document.ConsolidationReason, &document.OriginID); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(metadata), &document.Metadata)
		document.Vector = decodeVector(encodedVector)
		if document.Vector == nil {
			continue
		}
		documents = append(documents, document)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return documents, nil
}

func migrateSchema(database *sql.DB) error {
	columns := map[string]string{
		"status":               "TEXT NOT NULL DEFAULT 'active'",
		"superseded_by":        "TEXT NOT NULL DEFAULT ''",
		"superseded_at":        "TEXT NOT NULL DEFAULT ''",
		"consolidation_reason": "TEXT NOT NULL DEFAULT ''",
		"origin_id":            "TEXT NOT NULL DEFAULT ''",
	}
	rows, err := database.Query(`PRAGMA table_info(docs)`)
	if err != nil {
		return err
	}
	present := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, dataType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		present[name] = true
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for name, definition := range columns {
		if present[name] {
			continue
		}
		if _, err := database.Exec(`ALTER TABLE docs ADD COLUMN ` + name + ` ` + definition); err != nil {
			return err
		}
	}
	_, err = database.Exec(`CREATE INDEX IF NOT EXISTS docs_status_scope_project ON docs(status,scope,project);
CREATE INDEX IF NOT EXISTS docs_origin ON docs(origin_id);
CREATE INDEX IF NOT EXISTS knowledge_sources_key ON knowledge_sources(kind,source_key,project);`)
	return err
}

func encodeVector(vector []float32) []byte {
	encoded := make([]byte, len(vector)*4)
	for index, value := range vector {
		bits := math.Float32bits(value)
		offset := index * 4
		encoded[offset] = byte(bits)
		encoded[offset+1] = byte(bits >> 8)
		encoded[offset+2] = byte(bits >> 16)
		encoded[offset+3] = byte(bits >> 24)
	}
	return encoded
}

func decodeVector(encoded []byte) []float32 {
	if len(encoded)%4 != 0 {
		return nil
	}
	vector := make([]float32, len(encoded)/4)
	for index := range vector {
		offset := index * 4
		bits := uint32(encoded[offset]) | uint32(encoded[offset+1])<<8 | uint32(encoded[offset+2])<<16 | uint32(encoded[offset+3])<<24
		vector[index] = math.Float32frombits(bits)
	}
	return vector
}
