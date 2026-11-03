package service

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"ragfy/internal/api"
	"ragfy/internal/digest"
	"ragfy/internal/knowledge"
	"ragfy/internal/model"
)

const (
	digestSpineCharacters  = 6500
	minimumSpineCharacters = 500
	defaultQueueCapacity   = 32
)

type Service struct {
	repository knowledge.Repository
	models     model.Engine
	queue      chan api.HookPayload
	digestLock sync.Mutex
}

func New(repository knowledge.Repository, models model.Engine) *Service {
	return NewWithQueueCapacity(repository, models, defaultQueueCapacity)
}

func NewWithQueueCapacity(repository knowledge.Repository, models model.Engine, queueCapacity int) *Service {
	return &Service{repository: repository, models: models, queue: make(chan api.HookPayload, queueCapacity)}
}

func (service *Service) Routes(token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", authenticate(token, func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, map[string]any{"ok": true, "queued": len(service.queue)})
	}))
	mux.HandleFunc("POST /recall", authenticate(token, service.handleRecall))
	mux.HandleFunc("POST /digest", authenticate(token, service.handleDigest))
	mux.HandleFunc("POST /ingest", authenticate(token, service.handleIngest))
	return mux
}

func (service *Service) StartWorker(ctx context.Context) {
	go service.worker(ctx)
}

func authenticate(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		providedToken := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(providedToken), []byte(token)) != 1 {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(writer, request)
	}
}

func (service *Service) handleRecall(writer http.ResponseWriter, request *http.Request) {
	var payload api.HookPayload
	if json.NewDecoder(io.LimitReader(request.Body, 2<<20)).Decode(&payload) != nil {
		writeJSON(writer, map[string]string{})
		return
	}
	query := strings.TrimSpace(payload.Prompt)
	if len(query) < 12 {
		writeJSON(writer, map[string]string{})
		return
	}
	query = truncate(query, 1600)
	ctx, cancel := context.WithTimeout(request.Context(), 35*time.Second)
	defer cancel()
	queryInput := "Instruct: Given a coding assistant prompt, retrieve relevant passages from the user's knowledge base that help answer the prompt.\nQuery: " + query
	vectors, err := service.models.Embed(ctx, []string{queryInput})
	if err != nil {
		log.Printf("recall embedding: %v", err)
		writeJSON(writer, map[string]string{})
		return
	}
	hits, err := service.repository.Search(ctx, query, vectors[0], 4)
	if err != nil {
		log.Printf("recall search: %v", err)
		writeJSON(writer, map[string]string{})
		return
	}
	var contextText strings.Builder
	for index, hit := range hits {
		fmt.Fprintf(&contextText, "[%d] %s\n%s\n\n", index+1, hit.Doc.Title, hit.Doc.Content)
	}
	if contextText.Len() > 0 {
		contextText.WriteString("\n---\nKB RECALL: Retrieved from your local knowledge base. Use only when relevant.")
	}
	writeJSON(writer, map[string]string{"context": contextText.String()})
}

func (service *Service) handleDigest(writer http.ResponseWriter, request *http.Request) {
	var payload api.HookPayload
	if json.NewDecoder(io.LimitReader(request.Body, 2<<20)).Decode(&payload) != nil {
		http.Error(writer, "invalid payload", http.StatusBadRequest)
		return
	}
	select {
	case service.queue <- payload:
		writeJSON(writer, map[string]bool{"queued": true})
	default:
		http.Error(writer, "digest queue full", http.StatusServiceUnavailable)
	}
}

func (service *Service) handleIngest(writer http.ResponseWriter, request *http.Request) {
	var payload struct {
		Path string `json:"path"`
	}
	if json.NewDecoder(io.LimitReader(request.Body, 1<<20)).Decode(&payload) != nil || payload.Path == "" {
		http.Error(writer, "path is required", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 30*time.Minute)
	defer cancel()
	documentCount, err := service.IngestPath(ctx, payload.Path)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(writer, map[string]int{"documents": documentCount})
}

func (service *Service) IngestPath(ctx context.Context, root string) (int, error) {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return 0, err
	}
	var documents []knowledge.Document
	err = filepath.WalkDir(absoluteRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		extension := strings.ToLower(filepath.Ext(path))
		if extension != ".md" && extension != ".txt" && extension != ".rst" {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() > 40<<20 {
			return nil
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		body := strings.TrimSpace(string(contents))
		if body == "" {
			return nil
		}
		title := strings.TrimSpace(strings.TrimSuffix(filepath.Base(path), extension))
		for index, chunk := range splitChunks(body, 1200, 160) {
			documents = append(documents, knowledge.Document{Source: "documents", SourceID: fmt.Sprintf("%x-%d", sha256.Sum256([]byte(path)), index), Title: title, Content: chunk, Area: "documents", Metadata: map[string]any{"path": path, "parent": path}})
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return service.storeDocuments(ctx, documents)
}

func (service *Service) storeDocuments(ctx context.Context, documents []knowledge.Document) (int, error) {
	storedCount := 0
	for start := 0; start < len(documents); start += 32 {
		end := min(start+32, len(documents))
		texts := make([]string, end-start)
		for index, document := range documents[start:end] {
			texts[index] = document.Title + "\n" + document.Content
		}
		vectors, err := service.models.Embed(ctx, texts)
		if err != nil {
			return storedCount, err
		}
		for index := range vectors {
			document := &documents[start+index]
			document.Vector = vectors[index]
			document.TS = time.Now().UTC().Format(time.RFC3339)
			if err := service.repository.Upsert(ctx, *document); err != nil {
				return storedCount, err
			}
			storedCount++
		}
	}
	return storedCount, nil
}

func (service *Service) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case payload := <-service.queue:
			started := time.Now()
			log.Printf("digest %s: processing transcript", payload.SessionID)
			if err := service.Digest(ctx, payload); err != nil {
				log.Printf("digest %s: %v", payload.SessionID, err)
			} else {
				log.Printf("digest %s: completed in %s", payload.SessionID, time.Since(started).Round(time.Second))
			}
		}
	}
}

func RefreshEmbeddings(ctx context.Context, repository knowledge.Repository, models model.Engine, embeddingVersion string) error {
	if _, err := models.Embed(ctx, []string{"Ragfy local knowledge retrieval."}); err != nil {
		return fmt.Errorf("validate multilingual embedding model: %w", err)
	}
	version, err := repository.EmbeddingModel(ctx)
	if err != nil {
		return err
	}
	if version == embeddingVersion {
		return nil
	}
	documents, err := repository.Documents(ctx)
	if err != nil {
		return err
	}
	if len(documents) > 0 {
		log.Printf("startup: rebuilding %d local embeddings for the new multilingual model", len(documents))
	}
	for start := 0; start < len(documents); start += 8 {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(start+8, len(documents))
		texts := make([]string, end-start)
		for index, document := range documents[start:end] {
			texts[index] = document.Title + "\n" + document.Content
		}
		vectors, err := models.Embed(ctx, texts)
		if err != nil {
			return fmt.Errorf("rebuild embeddings: %w", err)
		}
		if len(vectors) != len(texts) {
			return fmt.Errorf("rebuild embeddings: expected %d vectors, got %d", len(texts), len(vectors))
		}
		for index, document := range documents[start:end] {
			if err := repository.UpdateEmbedding(ctx, document.Source, document.SourceID, vectors[index]); err != nil {
				return err
			}
		}
		log.Printf("startup: rebuilt embeddings: %d/%d", end, len(documents))
	}
	return repository.SetEmbeddingModel(ctx, embeddingVersion)
}

func (service *Service) Digest(ctx context.Context, payload api.HookPayload) error {
	if payload.Transcript == "" {
		return errors.New("transcript path missing")
	}
	transcript, err := os.ReadFile(payload.Transcript)
	if err != nil {
		return err
	}
	transcriptHash := SHA256Hex(transcript)
	service.digestLock.Lock()
	defer service.digestLock.Unlock()
	sessionID := payload.SessionID
	if sessionID == "" {
		sessionID = transcriptHash
	}
	alreadyDigested, err := service.repository.HasSession(ctx, sessionID, transcriptHash)
	if err != nil || alreadyDigested {
		return err
	}
	spine := digest.TranscriptSpine(transcript, digestSpineCharacters)
	if len(spine) < minimumSpineCharacters {
		if err := service.repository.DeleteSessionDocs(ctx, sessionID); err != nil {
			return err
		}
		_, err = service.repository.MarkSession(ctx, sessionID, transcriptHash)
		return err
	}
	project := filepath.Base(payload.CWD)
	if project == "." {
		project = "unknown"
	}
	documents, err := service.generateDocuments(ctx, sessionID, project, spine)
	if err != nil {
		return err
	}
	texts := make([]string, len(documents))
	for index, document := range documents {
		texts[index] = document.Title + "\n" + document.Content
	}
	vectors, err := service.models.Embed(ctx, texts)
	if err != nil {
		return err
	}
	if err := service.repository.DeleteSessionDocs(ctx, sessionID); err != nil {
		return err
	}
	for index := range documents {
		document := &documents[index]
		document.Vector = vectors[index]
		document.TS = time.Now().UTC().Format(time.RFC3339)
		document.SourceID = SHA256Hex([]byte(document.Kind + "\n" + document.Content))
		if document.Metadata == nil {
			document.Metadata = make(map[string]any)
		}
		document.Metadata["project"] = project
		document.Metadata["session_id"] = sessionID
		document.Metadata["transcript_hash"] = transcriptHash
		if err := service.repository.Upsert(ctx, *document); err != nil {
			return err
		}
	}
	_, err = service.repository.MarkSession(ctx, sessionID, transcriptHash)
	return err
}

func (service *Service) generateDocuments(ctx context.Context, sessionID, project, spine string) ([]knowledge.Document, error) {
	response, err := service.models.Generate(ctx, digest.Prompt(project, spine))
	if err != nil {
		return nil, err
	}
	documents, err := digest.ParseRecords(response, spine)
	if err == nil {
		return documents, nil
	}
	log.Printf("digest %s: invalid structured response (%v); retrying once with strict JSON instructions", sessionID, err)
	retryPrompt := "Return one valid JSON object only. It must start with { and end with }. Do not add analysis, explanations, or markdown.\n\n" + digest.Prompt(project, spine)
	response, err = service.models.Generate(ctx, retryPrompt)
	if err != nil {
		return nil, err
	}
	documents, err = digest.ParseRecords(response, spine)
	if err != nil {
		return nil, fmt.Errorf("digest model response did not contain valid JSON after retry: %w", err)
	}
	return documents, nil
}

func splitChunks(text string, target, overlap int) []string {
	if len(text) <= target {
		return []string{text}
	}
	var chunks []string
	start := 0
	for start < len(text) {
		end := start + target
		if end >= len(text) {
			chunks = append(chunks, strings.TrimSpace(text[start:]))
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
		chunks = append(chunks, strings.TrimSpace(text[start:end]))
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
	return chunks
}

func truncate(text string, maximumLength int) string {
	if len(text) <= maximumLength {
		return text
	}
	for maximumLength > 0 && maximumLength < len(text) && !utf8.RuneStart(text[maximumLength]) {
		maximumLength--
	}
	return text[:maximumLength]
}

func SHA256Hex(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func writeJSON(writer http.ResponseWriter, value any) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(value)
}
