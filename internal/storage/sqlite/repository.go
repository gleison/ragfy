package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

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

func (repository *Repository) Documents(ctx context.Context) ([]knowledge.Document, error) {
	rows, err := repository.database.QueryContext(ctx, `SELECT source,source_id,ts,title,content,metadata,kind,area FROM docs ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var documents []knowledge.Document
	for rows.Next() {
		var document knowledge.Document
		var metadata string
		if err := rows.Scan(&document.Source, &document.SourceID, &document.TS, &document.Title, &document.Content, &metadata, &document.Kind, &document.Area); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(metadata), &document.Metadata)
		documents = append(documents, document)
	}
	return documents, rows.Err()
}

func (repository *Repository) UpdateEmbedding(ctx context.Context, source, sourceID string, vector []float32) error {
	_, err := repository.database.ExecContext(ctx, `UPDATE docs SET embedding=? WHERE source=? AND source_id=?`, encodeVector(vector), source, sourceID)
	return err
}

func (repository *Repository) Upsert(ctx context.Context, document knowledge.Document) error {
	metadata, _ := json.Marshal(document.Metadata)
	var vector []byte
	if len(document.Vector) > 0 {
		vector = encodeVector(document.Vector)
	}
	_, err := repository.database.ExecContext(ctx, `INSERT INTO docs(source,source_id,ts,title,content,metadata,kind,area,embedding)
VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(source,source_id) DO UPDATE SET ts=excluded.ts,title=excluded.title,
content=excluded.content,metadata=excluded.metadata,kind=excluded.kind,area=excluded.area,embedding=excluded.embedding`,
		document.Source, document.SourceID, document.TS, document.Title, document.Content, string(metadata), document.Kind, document.Area, vector)
	return err
}

func (repository *Repository) MarkSession(ctx context.Context, sessionID, transcriptHash string) (bool, error) {
	if sessionID == "" {
		sessionID = transcriptHash
	}
	result, err := repository.database.ExecContext(ctx, `INSERT INTO digested_sessions(session_id,transcript_hash,created_at) VALUES(?,?,?)
ON CONFLICT(session_id) DO UPDATE SET transcript_hash=excluded.transcript_hash,created_at=excluded.created_at`, sessionID, transcriptHash, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return false, err
	}
	rowsAffected, err := result.RowsAffected()
	return rowsAffected == 1, err
}

func (repository *Repository) HasSession(ctx context.Context, sessionID, transcriptHash string) (bool, error) {
	var storedHash string
	err := repository.database.QueryRowContext(ctx, `SELECT transcript_hash FROM digested_sessions WHERE session_id=?`, sessionID).Scan(&storedHash)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return storedHash == transcriptHash, err
}

func (repository *Repository) DeleteSessionDocs(ctx context.Context, sessionID string) error {
	pattern := "%" + `"session_id":"` + sessionID + `"` + "%"
	_, err := repository.database.ExecContext(ctx, `DELETE FROM docs WHERE source='transcripts' AND metadata LIKE ?`, pattern)
	return err
}

func (repository *Repository) Search(ctx context.Context, query string, queryVector []float32, limit int) ([]knowledge.Hit, error) {
	rows, err := repository.database.QueryContext(ctx, `SELECT source,source_id,ts,title,content,metadata,kind,area,embedding FROM docs WHERE embedding IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	terms := queryTerms(query)
	hits := make([]knowledge.Hit, 0)
	for rows.Next() {
		var document knowledge.Document
		var metadata string
		var encodedVector []byte
		if err := rows.Scan(&document.Source, &document.SourceID, &document.TS, &document.Title, &document.Content, &metadata, &document.Kind, &document.Area, &encodedVector); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(metadata), &document.Metadata)
		document.Vector = decodeVector(encodedVector)
		if document.Vector == nil {
			continue
		}
		semanticScore := cosine(queryVector, document.Vector)
		lexicalMatch := lexicalScore(terms, document.Title+" "+document.Content)
		if semanticScore < .35 || (semanticScore < .50 && lexicalMatch == 0) {
			continue
		}
		hits = append(hits, knowledge.Hit{Score: semanticScore*.75 + lexicalMatch*.25, Doc: document})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortHits(hits)
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

func queryTerms(query string) []string {
	stopWords := map[string]bool{"the": true, "and": true, "for": true, "you": true, "your": true, "that": true, "this": true, "with": true, "from": true, "are": true, "was": true, "what": true, "how": true, "why": true, "can": true, "please": true, "about": true, "have": true, "has": true, "into": true, "not": true, "but": true, "use": true, "using": true, "want": true, "need": true, "make": true}
	var terms []string
	seen := map[string]bool{}
	for _, term := range strings.FieldsFunc(strings.ToLower(query), func(character rune) bool {
		return !(unicode.IsLetter(character) || unicode.IsNumber(character) || character == '_' || character == '-' || character == '.')
	}) {
		if len(term) < 3 || stopWords[term] || seen[term] {
			continue
		}
		terms = append(terms, term)
		seen[term] = true
	}
	return terms
}

func lexicalScore(terms []string, text string) float64 {
	if len(terms) == 0 {
		return 0
	}
	lowerText := strings.ToLower(text)
	matches := 0
	for _, term := range terms {
		if strings.Contains(lowerText, term) {
			matches++
		}
	}
	return float64(matches) / float64(len(terms))
}

func cosine(first, second []float32) float64 {
	if len(first) == 0 || len(first) != len(second) {
		return 0
	}
	var dotProduct, firstMagnitude, secondMagnitude float64
	for index := range first {
		firstValue, secondValue := float64(first[index]), float64(second[index])
		dotProduct += firstValue * secondValue
		firstMagnitude += firstValue * firstValue
		secondMagnitude += secondValue * secondValue
	}
	if firstMagnitude == 0 || secondMagnitude == 0 {
		return 0
	}
	return dotProduct / (firstMagnitude * secondMagnitude)
}

func sortHits(hits []knowledge.Hit) {
	for index := 1; index < len(hits); index++ {
		for position := index; position > 0 && hits[position].Score > hits[position-1].Score; position-- {
			hits[position], hits[position-1] = hits[position-1], hits[position]
		}
	}
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
