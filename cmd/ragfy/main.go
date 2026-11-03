package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	llama "github.com/mutablelogic/go-llama/pkg/llamacpp"
	"github.com/mutablelogic/go-llama/pkg/llamacpp/schema"
	llamasys "github.com/mutablelogic/go-llama/sys/llamacpp"
	_ "modernc.org/sqlite"
)

const (
	serviceName       = "ragfy"
	listenAddr        = "127.0.0.1:37841"
	chatModel         = "Qwen3-4B-Q4_K_M.gguf"
	chatURL           = "hf://Qwen/Qwen3-4B-GGUF/Qwen3-4B-Q4_K_M.gguf"
	chatSHA256        = "7485fe6f11af29433bc51cab58009521f205840f5b4ae3a32fa7f92e8534fdf5"
	embedModel        = "Qwen3-Embedding-0.6B-Q8_0.gguf"
	embedURL          = "hf://Qwen/Qwen3-Embedding-0.6B-GGUF/Qwen3-Embedding-0.6B-Q8_0.gguf"
	embedSHA256       = "06507c7b42688469c4e7298b0a1e16deff06caf291cf0a5b278c308249c3e439"
	embedModelVersion = embedSHA256 + ":query-instruction-v1"
	chatContextSize   = 4096
	chatBatchSize     = 2048
	chatOutputTokens  = 1200
	digestSpineChars  = 6500
	minimumSpineChars = 500
)

type HookPayload struct {
	Prompt        string `json:"prompt"`
	SessionID     string `json:"session_id"`
	Transcript    string `json:"transcript_path"`
	CWD           string `json:"cwd"`
	Event         string `json:"hook_event_name"`
	EventCodex    string `json:"event"`
	LastAssistant string `json:"last_assistant_message"`
}

type Document struct {
	Source   string         `json:"source"`
	SourceID string         `json:"source_id"`
	TS       string         `json:"ts"`
	Title    string         `json:"title"`
	Content  string         `json:"content"`
	Metadata map[string]any `json:"metadata"`
	Kind     string         `json:"kind"`
	Area     string         `json:"area"`
	Vector   []float32      `json:"-"`
}

type Hit struct {
	Score float64
	Doc   Document
}

type KnowledgeRepository interface {
	Upsert(context.Context, Document) error
	Search(context.Context, string, []float32, int) ([]Hit, error)
	EmbeddingModel(context.Context) (string, error)
	SetEmbeddingModel(context.Context, string) error
	Documents(context.Context) ([]Document, error)
	UpdateEmbedding(context.Context, string, string, []float32) error
	HasSession(context.Context, string, string) (bool, error)
	MarkSession(context.Context, string, string) (bool, error)
	DeleteSessionDocs(context.Context, string) error
	Close() error
}

type SQLiteRepository struct{ db *sql.DB }

func OpenSQLite(path string) (*SQLiteRepository, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=WAL;
CREATE TABLE IF NOT EXISTS docs (
 id INTEGER PRIMARY KEY, source TEXT NOT NULL, source_id TEXT NOT NULL,
 ts TEXT, title TEXT, content TEXT NOT NULL, metadata TEXT, kind TEXT, area TEXT,
 embedding BLOB, UNIQUE(source,source_id));
CREATE TABLE IF NOT EXISTS digested_sessions (
 session_id TEXT PRIMARY KEY, transcript_hash TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS app_metadata (
 key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS docs_source ON docs(source);
CREATE INDEX IF NOT EXISTS docs_kind ON docs(kind);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &SQLiteRepository{db: db}, nil
}

func (r *SQLiteRepository) Close() error { return r.db.Close() }

func (r *SQLiteRepository) EmbeddingModel(ctx context.Context) (string, error) {
	var model string
	err := r.db.QueryRowContext(ctx, `SELECT value FROM app_metadata WHERE key='embedding_model'`).Scan(&model)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return model, err
}

func (r *SQLiteRepository) SetEmbeddingModel(ctx context.Context, model string) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO app_metadata(key,value) VALUES('embedding_model',?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value`, model)
	return err
}

func (r *SQLiteRepository) Documents(ctx context.Context) ([]Document, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT source,source_id,ts,title,content,metadata,kind,area FROM docs ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var docs []Document
	for rows.Next() {
		var d Document
		var metadata string
		if err := rows.Scan(&d.Source, &d.SourceID, &d.TS, &d.Title, &d.Content, &metadata, &d.Kind, &d.Area); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(metadata), &d.Metadata)
		docs = append(docs, d)
	}
	return docs, rows.Err()
}

func (r *SQLiteRepository) UpdateEmbedding(ctx context.Context, source, sourceID string, vector []float32) error {
	blob := make([]byte, len(vector)*4)
	for i, f := range vector {
		putFloat(blob[i*4:], f)
	}
	_, err := r.db.ExecContext(ctx, `UPDATE docs SET embedding=? WHERE source=? AND source_id=?`, blob, source, sourceID)
	return err
}

func (r *SQLiteRepository) Upsert(ctx context.Context, d Document) error {
	meta, _ := json.Marshal(d.Metadata)
	var vec []byte
	if len(d.Vector) > 0 {
		vec = make([]byte, len(d.Vector)*4)
		for i, f := range d.Vector {
			putFloat(vec[i*4:], f)
		}
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO docs(source,source_id,ts,title,content,metadata,kind,area,embedding)
VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(source,source_id) DO UPDATE SET ts=excluded.ts,title=excluded.title,
content=excluded.content,metadata=excluded.metadata,kind=excluded.kind,area=excluded.area,embedding=excluded.embedding`,
		d.Source, d.SourceID, d.TS, d.Title, d.Content, string(meta), d.Kind, d.Area, vec)
	return err
}

func (r *SQLiteRepository) MarkSession(ctx context.Context, id, hash string) (bool, error) {
	if id == "" {
		id = hash
	}
	res, err := r.db.ExecContext(ctx, `INSERT INTO digested_sessions(session_id,transcript_hash,created_at) VALUES(?,?,?)
ON CONFLICT(session_id) DO UPDATE SET transcript_hash=excluded.transcript_hash,created_at=excluded.created_at`, id, hash, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (r *SQLiteRepository) HasSession(ctx context.Context, id, hash string) (bool, error) {
	var storedHash string
	err := r.db.QueryRowContext(ctx, `SELECT transcript_hash FROM digested_sessions WHERE session_id=?`, id).Scan(&storedHash)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return storedHash == hash, err
}

func (r *SQLiteRepository) DeleteSessionDocs(ctx context.Context, id string) error {
	pattern := "%" + `"session_id":"` + id + `"` + "%"
	_, err := r.db.ExecContext(ctx, `DELETE FROM docs WHERE source='transcripts' AND metadata LIKE ?`, pattern)
	return err
}

func (r *SQLiteRepository) Search(ctx context.Context, query string, qvec []float32, limit int) ([]Hit, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT source,source_id,ts,title,content,metadata,kind,area,embedding FROM docs WHERE embedding IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	terms := queryTerms(query)
	hits := make([]Hit, 0)
	for rows.Next() {
		var d Document
		var metadata string
		var blob []byte
		if err := rows.Scan(&d.Source, &d.SourceID, &d.TS, &d.Title, &d.Content, &metadata, &d.Kind, &d.Area, &blob); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(metadata), &d.Metadata)
		if len(blob)%4 != 0 {
			continue
		}
		d.Vector = make([]float32, len(blob)/4)
		for i := range d.Vector {
			d.Vector[i] = getFloat(blob[i*4:])
		}
		semantic := cosine(qvec, d.Vector)
		lexical := lexicalScore(terms, d.Title+" "+d.Content)
		if semantic < .35 || (semantic < .50 && lexical == 0) {
			continue
		}
		hits = append(hits, Hit{Score: semantic*.75 + lexical*.25, Doc: d})
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

type ModelEngine interface {
	Embed(context.Context, []string) ([][]float32, error)
	Generate(context.Context, string) (string, error)
	Close() error
}

type LlamaEngine struct {
	llm                 *llama.Llama
	chatName, embedName string
}

func NewLlamaEngine(ctx context.Context, dir string) (*LlamaEngine, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	client, err := llama.New(dir)
	if err != nil {
		return nil, err
	}
	engine := &LlamaEngine{llm: client, chatName: chatModel, embedName: embedModel}
	for _, m := range []struct{ name, url, sha256 string }{{chatModel, chatURL, chatSHA256}, {embedModel, embedURL, embedSHA256}} {
		model, modelErr := client.GetModel(ctx, m.name)
		if modelErr == nil {
			modelPath := filepath.Join(client.Store.Path(), model.Path)
			if verifyErr := verifyModel(modelPath, m.sha256); verifyErr != nil {
				_ = os.Remove(modelPath)
				modelErr = verifyErr
			}
		}
		if modelErr != nil {
			log.Printf("startup: downloading model %s", m.name)
			lastPct := -1
			model, err = client.PullModel(ctx, schema.PullModelRequest{URL: m.url}, func(_ string, got, total uint64) error {
				if total > 0 {
					pct := int(got * 100 / total)
					if pct > lastPct {
						lastPct = pct
						log.Printf("startup: download %s: %d%%", m.name, pct)
					}
				}
				return nil
			})
			if err != nil {
				client.Close()
				return nil, fmt.Errorf("download %s: %w", m.name, err)
			}
		}
		if err = verifyModel(filepath.Join(client.Store.Path(), model.Path), m.sha256); err != nil {
			_ = os.Remove(filepath.Join(client.Store.Path(), model.Path))
			client.Close()
			return nil, fmt.Errorf("modelo %s inválido: %w", m.name, err)
		}
		log.Printf("startup: verified model %s", m.name)
	}
	for _, name := range []string{chatModel, embedModel} {
		log.Printf("startup: loading model %s", name)
		if _, err := client.LoadModel(ctx, schema.LoadModelRequest{Name: name}); err != nil {
			client.Close()
			return nil, fmt.Errorf("load %s: %w", name, err)
		}
	}
	log.Println("startup: models loaded")
	return engine, nil
}

func (e *LlamaEngine) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	for i := range texts {
		texts[i] = truncate(texts[i], 1400)
	}
	embeddings := true
	contextSize, batchSize := uint32(512), uint32(512)
	req := schema.ContextRequest{LoadModelRequest: schema.LoadModelRequest{Name: e.embedName}, ContextSize: &contextSize, BatchSize: &batchSize, UBatchSize: &batchSize, Embeddings: &embeddings}
	var vectors [][]float32
	for start := 0; start < len(texts); start++ {
		end := start + 1
		err := e.llm.WithContext(ctx, req, func(_ context.Context, task *llama.Task) error {
			task.CachedModel().Lock()
			defer task.CachedModel().Unlock()
			if pooling := task.Context().PoolingType(); pooling != llamasys.PoolingLast {
				return fmt.Errorf("embedding model pooling must be 'last', got %s", pooling)
			}
			opts := llamasys.DefaultEmbeddingOptions()
			opts.Normalize = true
			batch, err := task.Context().ComputeEmbeddings(task.Model(), texts[start:end], opts)
			if err != nil {
				return err
			}
			vectors = append(vectors, batch.Embeddings...)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return vectors, nil
}

func (e *LlamaEngine) Generate(ctx context.Context, prompt string) (string, error) {
	contextSize := uint32(chatContextSize)
	batchSize := uint32(chatBatchSize)
	uBatchSize := uint32(chatBatchSize)
	returnText := ""
	err := e.llm.WithContext(ctx, schema.ContextRequest{
		LoadModelRequest: schema.LoadModelRequest{Name: e.chatName},
		ContextSize:      &contextSize,
		BatchSize:        &batchSize,
		UBatchSize:       &uBatchSize,
	}, func(_ context.Context, task *llama.Task) error {
		task.CachedModel().Lock()
		defer task.CachedModel().Unlock()

		promptText := qwenChatMLPrompt(prompt)
		tokenOpts := llamasys.DefaultTokenizeOptions()
		tokenOpts.AddSpecial = true
		tokenOpts.ParseSpecial = true
		tokens, err := task.Model().Tokenize(promptText, tokenOpts)
		if err != nil {
			return err
		}
		if len(tokens)+chatOutputTokens > int(contextSize) {
			return fmt.Errorf("digest prompt exceeds context budget: %d tokens", len(tokens))
		}
		for _, bounds := range tokenChunks(len(tokens), int(batchSize)) {
			last := bounds[1] == len(tokens)
			batch, err := llamasys.BatchFromTokens(tokens[bounds[0]:bounds[1]], int32(bounds[0]), 0, last)
			if err != nil {
				return err
			}
			err = batch.Decode(task.Context())
			batch.Close()
			if err != nil {
				return fmt.Errorf("digest prompt decode at token %d: %w", bounds[0], err)
			}
		}
		params := llamasys.DefaultSamplerParams()
		params.Temperature, params.TopP, params.TopK = 0.7, 0.8, 20
		sampler, err := llamasys.NewSampler(task.Model(), params)
		if err != nil {
			return err
		}
		defer sampler.Close()
		var output strings.Builder
		for i := 0; i < chatOutputTokens; i++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			token, err := sampler.Sample(task.Context(), -1)
			if err != nil {
				return err
			}
			if task.Model().IsEOG(token) {
				returnText = output.String()
				return nil
			}
			piece, err := task.Model().Detokenize([]llamasys.Token{token}, llamasys.DefaultDetokenizeOptions())
			if err != nil {
				return err
			}
			output.WriteString(piece)
			next, err := llamasys.BatchFromTokens([]llamasys.Token{token}, int32(len(tokens)+i), 0, true)
			if err != nil {
				return err
			}
			err = next.Decode(task.Context())
			next.Close()
			if err != nil {
				return fmt.Errorf("digest token decode: %w", err)
			}
		}
		return errors.New("digest output exceeded token budget")
	})
	return returnText, err
}

func qwenChatMLPrompt(userPrompt string) string {
	return "<|im_start|>user\n" + userPrompt + "<|im_end|>\n<|im_start|>assistant\n<think>\n\n</think>\n\n"
}

func tokenChunks(total, size int) [][2]int {
	if total <= 0 || size <= 0 {
		return nil
	}
	chunks := make([][2]int, 0, (total+size-1)/size)
	for start := 0; start < total; start += size {
		end := min(start+size, total)
		chunks = append(chunks, [2]int{start, end})
	}
	return chunks
}
func (e *LlamaEngine) Close() error { return e.llm.Close() }

type Service struct {
	repo   KnowledgeRepository
	models ModelEngine
	queue  chan HookPayload
	lock   sync.Mutex
}

func (s *Service) Routes(token string) http.Handler {
	mux := http.NewServeMux()
	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("GET /health", auth(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "queued": len(s.queue)})
	}))
	mux.HandleFunc("POST /recall", auth(s.handleRecall))
	mux.HandleFunc("POST /digest", auth(s.handleDigest))
	mux.HandleFunc("POST /ingest", auth(s.handleIngest))
	return mux
}

func (s *Service) handleRecall(w http.ResponseWriter, r *http.Request) {
	var p HookPayload
	if json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&p) != nil {
		writeJSON(w, map[string]string{})
		return
	}
	query := strings.TrimSpace(p.Prompt)
	if len(query) < 12 {
		writeJSON(w, map[string]string{})
		return
	}
	query = truncate(query, 1600)
	ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
	defer cancel()
	queryInput := "Instruct: Given a coding assistant prompt, retrieve relevant passages from the user's knowledge base that help answer the prompt.\nQuery: " + query
	vectors, err := s.models.Embed(ctx, []string{queryInput})
	if err != nil {
		log.Printf("recall embedding: %v", err)
		writeJSON(w, map[string]string{})
		return
	}
	hits, err := s.repo.Search(ctx, query, vectors[0], 4)
	if err != nil {
		log.Printf("recall search: %v", err)
		writeJSON(w, map[string]string{})
		return
	}
	var b strings.Builder
	for i, h := range hits {
		fmt.Fprintf(&b, "[%d] %s\n%s\n\n", i+1, h.Doc.Title, h.Doc.Content)
	}
	if b.Len() > 0 {
		b.WriteString("\n---\nKB RECALL: Retrieved from your local knowledge base. Use only when relevant.")
	}
	writeJSON(w, map[string]string{"context": b.String()})
}

func (s *Service) handleDigest(w http.ResponseWriter, r *http.Request) {
	var p HookPayload
	if json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&p) != nil {
		http.Error(w, "invalid payload", 400)
		return
	}
	select {
	case s.queue <- p:
		writeJSON(w, map[string]bool{"queued": true})
	default:
		http.Error(w, "digest queue full", http.StatusServiceUnavailable)
	}
}

func (s *Service) handleIngest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req) != nil || req.Path == "" {
		http.Error(w, "path is required", 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	n, err := s.ingestPath(ctx, req.Path)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]int{"documents": n})
}

func (s *Service) ingestPath(ctx context.Context, root string) (int, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return 0, err
	}
	count := 0
	var docs []Document
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".md" && ext != ".txt" && ext != ".rst" {
			return nil
		}
		info, e := entry.Info()
		if e != nil {
			return e
		}
		if info.Size() > 40<<20 {
			return nil
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		body := strings.TrimSpace(string(b))
		if body == "" {
			return nil
		}
		title := strings.TrimSpace(strings.TrimSuffix(filepath.Base(path), ext))
		parts := splitChunks(body, 1200, 160)
		for i, part := range parts {
			docs = append(docs, Document{Source: "documents", SourceID: fmt.Sprintf("%x-%d", sha256.Sum256([]byte(path)), i), Title: title, Content: part, Area: "documents", Metadata: map[string]any{"path": path, "parent": path}})
		}
		return nil
	})
	if err != nil {
		return count, err
	}
	for i := 0; i < len(docs); i += 32 {
		end := min(i+32, len(docs))
		texts := make([]string, end-i)
		for j, d := range docs[i:end] {
			texts[j] = d.Title + "\n" + d.Content
		}
		vecs, e := s.models.Embed(ctx, texts)
		if e != nil {
			return count, e
		}
		for j := range vecs {
			docs[i+j].Vector = vecs[j]
			docs[i+j].TS = time.Now().UTC().Format(time.RFC3339)
			if e = s.repo.Upsert(ctx, docs[i+j]); e != nil {
				return count, e
			}
			count++
		}
	}
	return count, nil
}

func (s *Service) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case p := <-s.queue:
			started := time.Now()
			log.Printf("digest %s: processing transcript", p.SessionID)
			if err := s.digest(ctx, p); err != nil {
				log.Printf("digest %s: %v", p.SessionID, err)
			} else {
				log.Printf("digest %s: completed in %s", p.SessionID, time.Since(started).Round(time.Second))
			}
		}
	}
}

func refreshEmbeddings(ctx context.Context, repo KnowledgeRepository, models ModelEngine) error {
	if err := validateEmbeddingModel(ctx, models); err != nil {
		return fmt.Errorf("validate multilingual embedding model: %w", err)
	}
	version, err := repo.EmbeddingModel(ctx)
	if err != nil {
		return err
	}
	if version == embedModelVersion {
		return nil
	}
	docs, err := repo.Documents(ctx)
	if err != nil {
		return err
	}
	if len(docs) > 0 {
		log.Printf("startup: rebuilding %d local embeddings for the new multilingual model", len(docs))
	}
	for start := 0; start < len(docs); start += 8 {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(start+8, len(docs))
		texts := make([]string, end-start)
		for i, doc := range docs[start:end] {
			texts[i] = doc.Title + "\n" + doc.Content
		}
		vectors, err := models.Embed(ctx, texts)
		if err != nil {
			return fmt.Errorf("rebuild embeddings: %w", err)
		}
		if len(vectors) != len(texts) {
			return fmt.Errorf("rebuild embeddings: expected %d vectors, got %d", len(texts), len(vectors))
		}
		for i, doc := range docs[start:end] {
			if err := repo.UpdateEmbedding(ctx, doc.Source, doc.SourceID, vectors[i]); err != nil {
				return err
			}
		}
		log.Printf("startup: rebuilt embeddings: %d/%d", end, len(docs))
	}
	return repo.SetEmbeddingModel(ctx, embedModelVersion)
}

func validateEmbeddingModel(ctx context.Context, models ModelEngine) error {
	_, err := models.Embed(ctx, []string{"Ragfy local knowledge retrieval."})
	return err
}

func (s *Service) digest(ctx context.Context, p HookPayload) error {
	if p.Transcript == "" {
		return errors.New("transcript path missing")
	}
	data, err := os.ReadFile(p.Transcript)
	if err != nil {
		return err
	}
	sum := sha256Hex(data)
	s.lock.Lock()
	defer s.lock.Unlock()
	key := p.SessionID
	if key == "" {
		key = sum
	}
	done, err := s.repo.HasSession(ctx, key, sum)
	if err != nil {
		return err
	}
	if done {
		return nil
	}
	spine := transcriptSpine(data, digestSpineChars)
	if len(spine) < minimumSpineChars {
		if err := s.repo.DeleteSessionDocs(ctx, key); err != nil {
			return err
		}
		_, err = s.repo.MarkSession(ctx, key, sum)
		return err
	}
	project := filepath.Base(p.CWD)
	if project == "." {
		project = "unknown"
	}
	art, err := s.models.Generate(ctx, digestPrompt(project, spine))
	if err != nil {
		return err
	}
	recs, err := parseRecords(art, spine)
	if err != nil {
		log.Printf("digest %s: invalid structured response (%v); retrying once with strict JSON instructions", key, err)
		retryPrompt := "Return one valid JSON object only. It must start with { and end with }. Do not add analysis, explanations, or markdown.\n\n" + digestPrompt(project, spine)
		art, err = s.models.Generate(ctx, retryPrompt)
		if err != nil {
			return err
		}
		recs, err = parseRecords(art, spine)
		if err != nil {
			return fmt.Errorf("digest model response did not contain valid JSON after retry: %w", err)
		}
	}
	texts := make([]string, len(recs))
	for i, d := range recs {
		texts[i] = d.Title + "\n" + d.Content
	}
	vecs, err := s.models.Embed(ctx, texts)
	if err != nil {
		return err
	}
	if err := s.repo.DeleteSessionDocs(ctx, key); err != nil {
		return err
	}
	for i := range recs {
		recs[i].Vector = vecs[i]
		recs[i].TS = time.Now().UTC().Format(time.RFC3339)
		recs[i].SourceID = sha256Hex([]byte(recs[i].Kind + "\n" + recs[i].Content))
		if recs[i].Metadata == nil {
			recs[i].Metadata = make(map[string]any)
		}
		recs[i].Metadata["project"] = project
		recs[i].Metadata["session_id"] = key
		recs[i].Metadata["transcript_hash"] = sum
		if err := s.repo.Upsert(ctx, recs[i]); err != nil {
			return err
		}
	}
	_, err = s.repo.MarkSession(ctx, key, sum)
	return err
}

func handleHook(action string) {
	payload, _ := io.ReadAll(io.LimitReader(os.Stdin, 4<<20))
	var p HookPayload
	if json.Unmarshal(payload, &p) != nil {
		return
	}
	endpoint := "/recall"
	if action == "digest" || strings.EqualFold(p.Event, "SessionEnd") || strings.EqualFold(p.EventCodex, "SessionEnd") {
		endpoint = "/digest"
	}
	dataDir, _ := appDataDir()
	token, _ := os.ReadFile(filepath.Join(dataDir, "token"))
	req, _ := http.NewRequest("POST", "http://"+listenAddr+endpoint, strings.NewReader(string(payload)))
	if req == nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Content-Type", "application/json")
	timeout := 4 * time.Second
	if endpoint == "/digest" {
		timeout = 2 * time.Second
	}
	client := http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var result struct {
		Context string `json:"context"`
	}
	if endpoint == "/recall" {
		if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result) == nil && result.Context != "" {
			_ = writePromptHookOutput(os.Stdout, result.Context)
		}
	}
}

func writePromptHookOutput(w io.Writer, context string) error {
	if strings.TrimSpace(context) == "" {
		return nil
	}
	return json.NewEncoder(w).Encode(map[string]any{
		"hookSpecificOutput": map[string]string{
			"hookEventName":     "UserPromptSubmit",
			"additionalContext": context,
		},
	})
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "hook" {
		action := ""
		if len(os.Args) > 2 {
			action = os.Args[2]
		}
		handleHook(action)
		return
	}
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "serve":
		if err := serve(); err != nil {
			log.Fatal(err)
		}
	case "install":
		if err := installService(); err != nil {
			log.Fatal(err)
		}
	case "uninstall":
		if err := uninstallService(); err != nil {
			log.Fatal(err)
		}
	case "status":
		if err := status(); err != nil {
			log.Fatal(err)
		}
	case "setup-hooks":
		if err := setupHooks(); err != nil {
			log.Fatal(err)
		}
	case "setup":
		if err := installService(); err != nil {
			log.Fatal(err)
		}
		if err := setupHooks(); err != nil {
			log.Fatal(err)
		}
	case "ingest":
		if len(os.Args) < 3 {
			log.Fatal("usage: ragfy ingest <folder>")
		}
		if err := requestIngest(os.Args[2]); err != nil {
			log.Fatal(err)
		}
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
}

func serve() error {
	dir, err := appDataDir()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	logDir := filepath.Join(dir, "logs")
	if err = os.MkdirAll(logDir, 0700); err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(logDir, "service.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	log.SetOutput(logFile)
	token, err := loadToken(filepath.Join(dir, "token"))
	if err != nil {
		return err
	}
	repo, err := OpenSQLite(filepath.Join(dir, "memory.sqlite"))
	if err != nil {
		return err
	}
	defer repo.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	log.Println("initializing local models; missing model files will be downloaded")
	models, err := NewLlamaEngine(ctx, filepath.Join(dir, "models"))
	if err != nil {
		log.Printf("startup failed: %v", err)
		return err
	}
	defer models.Close()
	log.Println("startup: validating embedding model and stored vectors")
	if err := refreshEmbeddings(ctx, repo, models); err != nil {
		log.Printf("startup failed: %v", err)
		return err
	}
	s := &Service{repo: repo, models: models, queue: make(chan HookPayload, 32)}
	go s.worker(ctx)
	server := &http.Server{Addr: listenAddr, Handler: s.Routes(token), ReadHeaderTimeout: 3 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = server.Shutdown(shutdown)
	}()
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Printf("startup failed: %v", err)
		return err
	}
	log.Printf("startup: ready; %s active at http://%s", serviceName, listenAddr)
	err = server.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func usage() {
	fmt.Println("ragfy {setup|serve|install|uninstall|status|setup-hooks|ingest <folder>|hook [recall|digest]}")
}

func appDataDir() (string, error) {
	if v := os.Getenv("RAGFY_HOME"); v != "" {
		return v, nil
	}
	if runtime.GOOS == "windows" {
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			return "", errors.New("LOCALAPPDATA not set")
		}
		return filepath.Join(base, "Ragfy"), nil
	}
	if runtime.GOOS == "darwin" {
		h, e := os.UserHomeDir()
		return filepath.Join(h, "Library", "Application Support", "Ragfy"), e
	}
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		h, e := os.UserHomeDir()
		if e != nil {
			return "", e
		}
		base = filepath.Join(h, ".local", "share")
	}
	return filepath.Join(base, "ragfy"), nil
}

func loadToken(path string) (string, error) {
	if b, e := os.ReadFile(path); e == nil {
		return strings.TrimSpace(string(b)), nil
	}
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	token := hex.EncodeToString(b)
	if e := os.WriteFile(path, []byte(token+"\n"), 0600); e != nil {
		return "", e
	}
	return token, nil
}

func status() error {
	ctx, c := context.WithTimeout(context.Background(), 2*time.Second)
	defer c()
	dir, e := appDataDir()
	if e != nil {
		return e
	}
	token, e := os.ReadFile(filepath.Join(dir, "token"))
	if e != nil {
		return errors.New("daemon not configured; run ragfy install")
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+listenAddr+"/health", nil)
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	resp, e := http.DefaultClient.Do(req)
	if e != nil {
		if progress, ok := startupProgress(filepath.Join(dir, "logs", "service.log")); ok {
			fmt.Println(progress)
			return nil
		}
		return errors.New("daemon is not running")
	}
	defer resp.Body.Close()
	_, e = io.Copy(os.Stdout, resp.Body)
	return e
}

func startupProgress(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", false
	}
	const tailSize = 128 << 10
	start := info.Size() - tailSize
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return "", false
	}
	data, err := io.ReadAll(io.LimitReader(f, tailSize))
	if err != nil {
		return "", false
	}
	lines := strings.Split(string(data), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		stamp, message := "", line
		if len(line) > 20 {
			if _, err := time.ParseInLocation("2006/01/02 15:04:05", line[:19], time.Local); err == nil {
				stamp, message = line[:19], strings.TrimSpace(line[19:])
			}
		}
		if strings.Contains(message, "startup: ready") {
			return "", false
		}
		isProgress := strings.Contains(message, "startup:") || strings.Contains(message, "download ") || strings.Contains(message, "baixando modelo ")
		if !isProgress {
			continue
		}
		message = strings.TrimPrefix(message, "startup:")
		message = strings.TrimSpace(message)
		if stamp != "" {
			if t, err := time.ParseInLocation("2006/01/02 15:04:05", stamp, time.Local); err == nil {
				age := time.Since(t).Round(time.Second)
				if age < 0 {
					age = 0
				}
				return fmt.Sprintf("Service is not ready; last startup update %s ago: %s", age, message), true
			}
		}
		return "Service is not ready; last startup update: " + message, true
	}
	return "", false
}

func requestIngest(path string) error {
	dir, e := appDataDir()
	if e != nil {
		return e
	}
	token, e := os.ReadFile(filepath.Join(dir, "token"))
	if e != nil {
		return errors.New("service is not installed; run ragfy setup")
	}
	body, _ := json.Marshal(map[string]string{"path": path})
	req, e := http.NewRequest("POST", "http://"+listenAddr+"/ingest", strings.NewReader(string(body)))
	if e != nil {
		return e
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Content-Type", "application/json")
	client := http.Client{Timeout: 30 * time.Minute}
	resp, e := client.Do(req)
	if e != nil {
		return errors.New("daemon is not running; run ragfy install")
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("ingest failed: %s", strings.TrimSpace(string(b)))
	}
	fmt.Println(string(b))
	return nil
}

func installService() error {
	exe, e := os.Executable()
	if e != nil {
		return e
	}
	home, e := os.UserHomeDir()
	if e != nil {
		return e
	}
	switch runtime.GOOS {
	case "darwin":
		dir := filepath.Join(home, "Library", "LaunchAgents")
		if e = os.MkdirAll(dir, 0700); e != nil {
			return e
		}
		p := filepath.Join(dir, "com.ragfy.service.plist")
		content := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>Label</key><string>com.ragfy.service</string><key>ProgramArguments</key><array><string>%s</string><string>serve</string></array><key>RunAtLoad</key><true/><key>KeepAlive</key><true/></dict></plist>`, exe)
		if e = os.WriteFile(p, []byte(content), 0600); e != nil {
			return e
		}
		return exec.Command("launchctl", "load", p).Run()
	case "linux":
		dir := filepath.Join(home, ".config", "systemd", "user")
		if e = os.MkdirAll(dir, 0700); e != nil {
			return e
		}
		content := fmt.Sprintf("[Unit]\nDescription=Ragfy local memory service\nAfter=default.target\n\n[Service]\nExecStart=%s serve\nRestart=on-failure\n\n[Install]\nWantedBy=default.target\n", exe)
		if e = os.WriteFile(filepath.Join(dir, "ragfy.service"), []byte(content), 0600); e != nil {
			return e
		}
		if e = exec.Command("systemctl", "--user", "daemon-reload").Run(); e != nil {
			return e
		}
		return exec.Command("systemctl", "--user", "enable", "--now", "ragfy.service").Run()
	case "windows":
		args := fmt.Sprintf(`"%s" serve`, exe)
		if e = exec.Command("schtasks", "/Create", "/F", "/SC", "ONLOGON", "/TN", "RagfyService", "/TR", args).Run(); e != nil {
			return e
		}
		return exec.Command("schtasks", "/Run", "/TN", "RagfyService").Run()
	default:
		return fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
}

func uninstallService() error {
	home, e := os.UserHomeDir()
	if e != nil {
		return e
	}
	switch runtime.GOOS {
	case "darwin":
		p := filepath.Join(home, "Library", "LaunchAgents", "com.ragfy.service.plist")
		_ = exec.Command("launchctl", "unload", p).Run()
		return os.Remove(p)
	case "linux":
		_ = exec.Command("systemctl", "--user", "disable", "--now", "ragfy.service").Run()
		p := filepath.Join(home, ".config", "systemd", "user", "ragfy.service")
		_ = os.Remove(p)
		return exec.Command("systemctl", "--user", "daemon-reload").Run()
	case "windows":
		return exec.Command("schtasks", "/Delete", "/F", "/TN", "RagfyService").Run()
	}
	return nil
}

func setupHooks() error {
	home, e := os.UserHomeDir()
	if e != nil {
		return e
	}
	exe, e := os.Executable()
	if e != nil {
		return e
	}
	for _, p := range []string{filepath.Join(home, ".claude", "settings.json"), filepath.Join(home, ".codex", "hooks.json")} {
		if e = mergeHookFile(p, exe); e != nil {
			return e
		}
		fmt.Println("Hooks atualizados:", p)
	}
	return nil
}

func mergeHookFile(path, exe string) error {
	var root map[string]any
	if b, e := os.ReadFile(path); e == nil {
		if json.Unmarshal(b, &root) != nil {
			return fmt.Errorf("invalid hook config: %s", path)
		}
		backup := path + ".ragfy.bak"
		if _, e := os.Stat(backup); os.IsNotExist(e) {
			_ = os.WriteFile(backup, b, 0600)
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	if root == nil {
		root = map[string]any{}
	}
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		root["hooks"] = hooks
	}
	removeLegacyPythonRecallHooks(hooks)
	for event, action := range map[string]string{"UserPromptSubmit": "recall", "SessionEnd": "digest"} {
		command := strconv.Quote(exe) + " hook " + action
		timeout := 4
		if event == "SessionEnd" {
			timeout = 2
		}
		list, _ := hooks[event].([]any)
		exists := false
		for _, raw := range list {
			entry, _ := raw.(map[string]any)
			hs, _ := entry["hooks"].([]any)
			for _, h := range hs {
				hm, _ := h.(map[string]any)
				if hm["command"] == command {
					exists = true
				}
			}
		}
		if !exists {
			list = append(list, map[string]any{"hooks": []any{map[string]any{"type": "command", "command": command, "timeout": timeout}}})
			hooks[event] = list
		}
	}
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	b, e := json.MarshalIndent(root, "", "  ")
	if e != nil {
		return e
	}
	return os.WriteFile(path, append(b, '\n'), 0600)
}

func removeLegacyPythonRecallHooks(hooks map[string]any) {
	entries, ok := hooks["UserPromptSubmit"].([]any)
	if !ok {
		return
	}
	keptEntries := make([]any, 0, len(entries))
	for _, rawEntry := range entries {
		entry, ok := rawEntry.(map[string]any)
		if !ok {
			keptEntries = append(keptEntries, rawEntry)
			continue
		}
		children, _ := entry["hooks"].([]any)
		keptHooks := make([]any, 0, len(children))
		for _, child := range children {
			hook, _ := child.(map[string]any)
			command, _ := hook["command"].(string)
			if !strings.Contains(command, "recall.py") {
				keptHooks = append(keptHooks, child)
			}
		}
		if len(keptHooks) > 0 {
			entry["hooks"] = keptHooks
			keptEntries = append(keptEntries, entry)
		}
	}
	hooks["UserPromptSubmit"] = keptEntries
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func queryTerms(s string) []string {
	stop := map[string]bool{"the": true, "and": true, "for": true, "you": true, "your": true, "that": true, "this": true, "with": true, "from": true, "are": true, "was": true, "what": true, "how": true, "why": true, "can": true, "please": true, "about": true, "have": true, "has": true, "into": true, "not": true, "but": true, "use": true, "using": true, "want": true, "need": true, "make": true}
	out := []string{}
	seen := map[string]bool{}
	for _, t := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' || r == '-' || r == '.')
	}) {
		if len(t) < 3 || stop[t] || seen[t] {
			continue
		}
		out = append(out, t)
		seen[t] = true
	}
	return out
}
func splitChunks(text string, target, overlap int) []string {
	if len(text) <= target {
		return []string{text}
	}
	var out []string
	start := 0
	for start < len(text) {
		end := start + target
		if end >= len(text) {
			out = append(out, strings.TrimSpace(text[start:]))
			break
		}
		cut := strings.LastIndex(text[start:end], "\n\n")
		if cut < target/2 {
			cut = strings.LastIndex(text[start:end], ". ")
		}
		if cut < target/2 {
			cut = target
		}
		end = start + cut
		for end > start && end < len(text) && !utf8.RuneStart(text[end]) {
			end--
		}
		out = append(out, strings.TrimSpace(text[start:end]))
		start = end - overlap
		if start < 0 {
			start = 0
		}
		for start > 0 && start < len(text) && !utf8.RuneStart(text[start]) {
			start++
		}
		for start < len(text) && (text[start] == ' ' || text[start] == '\n') {
			start++
		}
	}
	return out
}
func lexicalScore(terms []string, text string) float64 {
	if len(terms) == 0 {
		return 0
	}
	low := strings.ToLower(text)
	n := 0
	for _, t := range terms {
		if strings.Contains(low, t) {
			n++
		}
	}
	return float64(n) / float64(len(terms))
}
func cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, aa, bb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		aa += x * x
		bb += y * y
	}
	if aa == 0 || bb == 0 {
		return 0
	}
	return dot / (aa * bb)
}
func sortHits(h []Hit) {
	for i := 1; i < len(h); i++ {
		for j := i; j > 0 && h[j].Score > h[j-1].Score; j-- {
			h[j], h[j-1] = h[j-1], h[j]
		}
	}
}
func putFloat(b []byte, v float32) {
	u := math.Float32bits(v)
	b[0] = byte(u)
	b[1] = byte(u >> 8)
	b[2] = byte(u >> 16)
	b[3] = byte(u >> 24)
}
func getFloat(b []byte) float32 {
	return math.Float32frombits(uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24)
}
func sha256Hex(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

func verifyModel(path, expected string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != expected {
		return fmt.Errorf("SHA-256 mismatch: got %s", got)
	}
	return nil
}
