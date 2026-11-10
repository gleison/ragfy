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

func (repository *Repository) Sessions(ctx context.Context) ([]knowledge.Session, error) {
	rows, err := repository.database.QueryContext(ctx, `SELECT
 ds.session_id,ds.transcript_hash,ds.created_at,
 COALESCE((SELECT provider FROM knowledge_sources WHERE kind='session' AND source_key=ds.session_id ORDER BY created_at DESC,id DESC LIMIT 1),''),
 COALESCE((SELECT project FROM knowledge_sources WHERE kind='session' AND source_key=ds.session_id ORDER BY created_at DESC,id DESC LIMIT 1),''),
 (SELECT COUNT(*) FROM knowledge_sources WHERE kind='session' AND source_key=ds.session_id),
 (SELECT COUNT(*) FROM docs WHERE status='active' AND (origin_id IN (SELECT id FROM knowledge_sources WHERE kind='session' AND source_key=ds.session_id) OR (origin_id='' AND json_extract(metadata,'$.session_id')=ds.session_id))),
 (SELECT COUNT(*) FROM docs WHERE status='superseded' AND (origin_id IN (SELECT id FROM knowledge_sources WHERE kind='session' AND source_key=ds.session_id) OR (origin_id='' AND json_extract(metadata,'$.session_id')=ds.session_id)))
 FROM digested_sessions ds ORDER BY ds.created_at DESC,ds.session_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []knowledge.Session
	for rows.Next() {
		var session knowledge.Session
		if err := rows.Scan(&session.ID, &session.TranscriptHash, &session.DigestedAt, &session.Provider, &session.Project, &session.SourceVersions, &session.ActiveRecords, &session.SupersededRecords); err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

func (repository *Repository) Session(ctx context.Context, sessionID string) (knowledge.Session, error) {
	var session knowledge.Session
	err := repository.database.QueryRowContext(ctx, `SELECT session_id,transcript_hash,created_at FROM digested_sessions WHERE session_id=?`, sessionID).Scan(&session.ID, &session.TranscriptHash, &session.DigestedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return session, knowledge.ErrNotFound
	}
	if err != nil {
		return session, err
	}
	sourceRows, err := repository.database.QueryContext(ctx, `SELECT id,kind,source_key,provider,project,title,content,content_hash,created_at FROM knowledge_sources WHERE kind='session' AND source_key=? ORDER BY created_at,id`, sessionID)
	if err != nil {
		return session, err
	}
	for sourceRows.Next() {
		var source knowledge.Source
		if err := sourceRows.Scan(&source.ID, &source.Kind, &source.Key, &source.Provider, &source.Project, &source.Title, &source.Content, &source.ContentHash, &source.CreatedAt); err != nil {
			sourceRows.Close()
			return session, err
		}
		session.Sources = append(session.Sources, source)
	}
	if err := sourceRows.Err(); err != nil {
		sourceRows.Close()
		return session, err
	}
	if err := sourceRows.Close(); err != nil {
		return session, err
	}
	documentRows, err := repository.database.QueryContext(ctx, `SELECT source,source_id,ts,title,content,metadata,kind,area,scope,project,embedding,status,superseded_by,superseded_at,consolidation_reason,origin_id FROM docs WHERE origin_id IN (SELECT id FROM knowledge_sources WHERE kind='session' AND source_key=?) OR (origin_id='' AND json_extract(metadata,'$.session_id')=?) ORDER BY id`, sessionID, sessionID)
	if err != nil {
		return session, err
	}
	defer documentRows.Close()
	for documentRows.Next() {
		document, err := scanDocument(documentRows)
		if err != nil {
			return session, err
		}
		session.Documents = append(session.Documents, document)
		if document.Status == knowledge.StatusActive {
			session.ActiveRecords++
		} else if document.Status == knowledge.StatusSuperseded {
			session.SupersededRecords++
		}
	}
	if err := documentRows.Err(); err != nil {
		return session, err
	}
	session.SourceVersions = len(session.Sources)
	if len(session.Sources) > 0 {
		latest := session.Sources[len(session.Sources)-1]
		session.Provider = latest.Provider
		session.Project = latest.Project
	}
	return session, nil
}

func (repository *Repository) ForgetSession(ctx context.Context, sessionID string) (knowledge.ForgetResult, error) {
	result := knowledge.ForgetResult{SessionID: sessionID}
	transaction, err := repository.database.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer transaction.Rollback()
	var exists int
	if err := transaction.QueryRowContext(ctx, `SELECT COUNT(*) FROM digested_sessions WHERE session_id=?`, sessionID).Scan(&exists); err != nil {
		return result, err
	}
	if exists == 0 {
		return result, knowledge.ErrNotFound
	}
	if err := transaction.QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge_sources WHERE kind='session' AND source_key=?`, sessionID).Scan(&result.RemovedSources); err != nil {
		return result, err
	}
	if err := transaction.QueryRowContext(ctx, `SELECT COUNT(*) FROM docs WHERE origin_id IN (SELECT id FROM knowledge_sources WHERE kind='session' AND source_key=?) OR (origin_id='' AND json_extract(metadata,'$.session_id')=?)`, sessionID, sessionID).Scan(&result.RemovedRecords); err != nil {
		return result, err
	}
	if err := transaction.QueryRowContext(ctx, `SELECT COUNT(*) FROM recall_events WHERE session_id=?`, sessionID).Scan(&result.RemovedRecalls); err != nil {
		return result, err
	}
	reactivated, err := transaction.ExecContext(ctx, `UPDATE docs SET status='active',superseded_by='',superseded_at='',consolidation_reason='' WHERE status='superseded' AND superseded_by IN (
 SELECT source_id FROM docs WHERE origin_id IN (SELECT id FROM knowledge_sources WHERE kind='session' AND source_key=?) OR (origin_id='' AND json_extract(metadata,'$.session_id')=?))
 AND NOT (origin_id IN (SELECT id FROM knowledge_sources WHERE kind='session' AND source_key=?) OR (origin_id='' AND json_extract(metadata,'$.session_id')=?))`, sessionID, sessionID, sessionID, sessionID)
	if err != nil {
		return result, err
	}
	if count, err := reactivated.RowsAffected(); err == nil {
		result.ReactivatedRecords = int(count)
	}
	if _, err := transaction.ExecContext(ctx, `DELETE FROM docs WHERE origin_id IN (SELECT id FROM knowledge_sources WHERE kind='session' AND source_key=?) OR (origin_id='' AND json_extract(metadata,'$.session_id')=?)`, sessionID, sessionID); err != nil {
		return result, err
	}
	if _, err := transaction.ExecContext(ctx, `DELETE FROM knowledge_sources WHERE kind='session' AND source_key=?`, sessionID); err != nil {
		return result, err
	}
	if _, err := transaction.ExecContext(ctx, `DELETE FROM recall_results WHERE recall_id IN (SELECT id FROM recall_events WHERE session_id=?)`, sessionID); err != nil {
		return result, err
	}
	if _, err := transaction.ExecContext(ctx, `DELETE FROM recall_events WHERE session_id=?`, sessionID); err != nil {
		return result, err
	}
	if _, err := transaction.ExecContext(ctx, `DELETE FROM digested_sessions WHERE session_id=?`, sessionID); err != nil {
		return result, err
	}
	if err := transaction.Commit(); err != nil {
		return result, err
	}
	return result, nil
}

func (repository *Repository) RecordRecall(ctx context.Context, event knowledge.RecallEvent) error {
	transaction, err := repository.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	if _, err := transaction.ExecContext(ctx, `INSERT INTO recall_events(id,session_id,source,project,prompt,created_at,score_cutoff,lexical_cutoff) VALUES(?,?,?,?,?,?,?,?)`, event.ID, event.SessionID, event.Source, event.Project, event.Prompt, event.CreatedAt, event.ScoreCutoff, event.LexicalCutoff); err != nil {
		return err
	}
	for _, result := range event.Results {
		if result.Status == "" {
			result.Status = "selected"
		}
		if result.Status == "selected" && result.SelectedRank == 0 {
			result.SelectedRank = result.Rank
		}
		if _, err := transaction.ExecContext(ctx, `INSERT INTO recall_results(recall_id,rank,source_id,title,content,kind,scope,project,score,semantic_score,lexical_score,project_boost,selected_rank,status,feedback) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, event.ID, result.Rank, result.SourceID, result.Title, result.Content, result.Kind, result.Scope, result.Project, result.Score, result.SemanticScore, result.LexicalScore, result.ProjectBoost, result.SelectedRank, result.Status, result.Feedback); err != nil {
			return err
		}
	}
	return transaction.Commit()
}

func (repository *Repository) RecallEvents(ctx context.Context) ([]knowledge.RecallEvent, error) {
	rows, err := repository.database.QueryContext(ctx, `SELECT e.id,e.session_id,e.source,e.project,e.prompt,e.created_at,e.score_cutoff,e.lexical_cutoff,COUNT(r.rank) FILTER (WHERE r.status='selected'),COUNT(r.rank) FROM recall_events e LEFT JOIN recall_results r ON r.recall_id=e.id GROUP BY e.id ORDER BY e.created_at DESC,e.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []knowledge.RecallEvent
	for rows.Next() {
		var event knowledge.RecallEvent
		if err := rows.Scan(&event.ID, &event.SessionID, &event.Source, &event.Project, &event.Prompt, &event.CreatedAt, &event.ScoreCutoff, &event.LexicalCutoff, &event.ResultCount, &event.CandidateCount); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (repository *Repository) RecallEvent(ctx context.Context, recallID string) (knowledge.RecallEvent, error) {
	var event knowledge.RecallEvent
	err := repository.database.QueryRowContext(ctx, `SELECT id,session_id,source,project,prompt,created_at,score_cutoff,lexical_cutoff FROM recall_events WHERE id=?`, recallID).Scan(&event.ID, &event.SessionID, &event.Source, &event.Project, &event.Prompt, &event.CreatedAt, &event.ScoreCutoff, &event.LexicalCutoff)
	if errors.Is(err, sql.ErrNoRows) {
		return event, knowledge.ErrNotFound
	}
	if err != nil {
		return event, err
	}
	rows, err := repository.database.QueryContext(ctx, `SELECT rank,selected_rank,status,feedback,source_id,title,content,kind,scope,project,score,semantic_score,lexical_score,project_boost FROM recall_results WHERE recall_id=? ORDER BY rank`, recallID)
	if err != nil {
		return event, err
	}
	defer rows.Close()
	for rows.Next() {
		result, err := scanRecallResult(rows)
		if err != nil {
			return event, err
		}
		event.Results = append(event.Results, result)
	}
	for _, result := range event.Results {
		if result.Status == "selected" {
			event.ResultCount++
		}
	}
	event.CandidateCount = len(event.Results)
	return event, rows.Err()
}

func (repository *Repository) SetRecallFeedback(ctx context.Context, recallID string, rank int, feedback string) (knowledge.RecallResult, error) {
	if feedback != "useful" && feedback != "irrelevant" {
		return knowledge.RecallResult{}, errors.New("feedback must be useful or irrelevant")
	}
	result, err := repository.database.ExecContext(ctx, `UPDATE recall_results SET feedback=? WHERE recall_id=? AND rank=? AND status='selected'`, feedback, recallID, rank)
	if err != nil {
		return knowledge.RecallResult{}, err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return knowledge.RecallResult{}, err
	}
	if updated == 0 {
		return knowledge.RecallResult{}, knowledge.ErrNotFound
	}
	row := repository.database.QueryRowContext(ctx, `SELECT rank,selected_rank,status,feedback,source_id,title,content,kind,scope,project,score,semantic_score,lexical_score,project_boost FROM recall_results WHERE recall_id=? AND rank=?`, recallID, rank)
	return scanRecallResult(row)
}

func scanRecallResult(scanner documentScanner) (knowledge.RecallResult, error) {
	var result knowledge.RecallResult
	err := scanner.Scan(&result.Rank, &result.SelectedRank, &result.Status, &result.Feedback, &result.SourceID, &result.Title, &result.Content, &result.Kind, &result.Scope, &result.Project, &result.Score, &result.SemanticScore, &result.LexicalScore, &result.ProjectBoost)
	return result, err
}

func (repository *Repository) Documents(ctx context.Context) ([]knowledge.Document, error) {
	rows, err := repository.database.QueryContext(ctx, `SELECT source,source_id,ts,title,content,metadata,kind,area,scope,project,embedding,status,superseded_by,superseded_at,consolidation_reason,origin_id FROM docs ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var documents []knowledge.Document
	for rows.Next() {
		document, err := scanDocument(rows)
		if err != nil {
			return nil, err
		}
		documents = append(documents, document)
	}
	return documents, rows.Err()
}

type documentScanner interface {
	Scan(...any) error
}

func scanDocument(scanner documentScanner) (knowledge.Document, error) {
	var document knowledge.Document
	var metadata string
	var encodedVector []byte
	if err := scanner.Scan(&document.Source, &document.SourceID, &document.TS, &document.Title, &document.Content, &metadata, &document.Kind, &document.Area, &document.Scope, &document.Project, &encodedVector, &document.Status, &document.SupersededBy, &document.SupersededAt, &document.ConsolidationReason, &document.OriginID); err != nil {
		return document, err
	}
	_ = json.Unmarshal([]byte(metadata), &document.Metadata)
	document.Vector = decodeVector(encodedVector)
	return document, nil
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

func (repository *Repository) ProjectContext(ctx context.Context, project string, limit int) ([]knowledge.Document, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := repository.database.QueryContext(ctx, `SELECT source,source_id,ts,title,content,metadata,kind,area,scope,project,status,superseded_by,superseded_at,consolidation_reason,origin_id
FROM docs
WHERE status='active' AND scope='project' AND project=?
ORDER BY CASE WHEN ts='' THEN 1 ELSE 0 END,ts DESC,id DESC
LIMIT ?`, project, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	documents := make([]knowledge.Document, 0, limit)
	for rows.Next() {
		var document knowledge.Document
		var metadata string
		if err := rows.Scan(&document.Source, &document.SourceID, &document.TS, &document.Title, &document.Content, &metadata, &document.Kind, &document.Area, &document.Scope, &document.Project, &document.Status, &document.SupersededBy, &document.SupersededAt, &document.ConsolidationReason, &document.OriginID); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(metadata), &document.Metadata)
		documents = append(documents, document)
	}
	return documents, rows.Err()
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
	if err := migrateColumns(database, "recall_events", map[string]string{
		"score_cutoff":   "REAL NOT NULL DEFAULT 0",
		"lexical_cutoff": "REAL NOT NULL DEFAULT 0",
	}); err != nil {
		return err
	}
	if err := migrateColumns(database, "recall_results", map[string]string{
		"selected_rank": "INTEGER NOT NULL DEFAULT 0",
		"status":        "TEXT NOT NULL DEFAULT 'selected'",
		"feedback":      "TEXT NOT NULL DEFAULT ''",
	}); err != nil {
		return err
	}
	if _, err := database.Exec(`UPDATE recall_results SET selected_rank=rank WHERE status='selected' AND selected_rank=0`); err != nil {
		return err
	}
	_, err = database.Exec(`CREATE INDEX IF NOT EXISTS docs_status_scope_project ON docs(status,scope,project);
CREATE INDEX IF NOT EXISTS docs_origin ON docs(origin_id);
CREATE INDEX IF NOT EXISTS knowledge_sources_key ON knowledge_sources(kind,source_key,project);`)
	return err
}

func migrateColumns(database *sql.DB, table string, columns map[string]string) error {
	rows, err := database.Query(`PRAGMA table_info(` + table + `)`)
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
		if _, err := database.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + name + ` ` + definition); err != nil {
			return err
		}
	}
	return nil
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
