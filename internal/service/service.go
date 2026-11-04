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
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"ragfy/internal/api"
	"ragfy/internal/chunking"
	"ragfy/internal/digest"
	"ragfy/internal/extraction"
	"ragfy/internal/knowledge"
	"ragfy/internal/model"
	"ragfy/internal/reconciliation"
	"ragfy/internal/retrieval"
)

const (
	minimumSpineCharacters = 500
	defaultQueueCapacity   = 32
)

type Service struct {
	repository knowledge.Repository
	embedder   model.Embedder
	extractor  extraction.Extractor
	retriever  retrieval.Retriever
	reconciler reconciliation.Reconciler
	chunker    chunking.Chunker
	queue      chan api.HookPayload
	digestLock sync.Mutex
}

type Dependencies struct {
	Repository knowledge.Repository
	Embedder   model.Embedder
	Extractor  extraction.Extractor
	Retriever  retrieval.Retriever
	Reconciler reconciliation.Reconciler
	Chunker    chunking.Chunker
}

func New(dependencies Dependencies) *Service {
	return NewWithQueueCapacity(dependencies, defaultQueueCapacity)
}

func NewWithQueueCapacity(dependencies Dependencies, queueCapacity int) *Service {
	reconciler := dependencies.Reconciler
	if reconciler == nil {
		reconciler = reconciliation.None{}
	}
	return &Service{
		repository: dependencies.Repository,
		embedder:   dependencies.Embedder,
		extractor:  dependencies.Extractor,
		retriever:  dependencies.Retriever,
		reconciler: reconciler,
		chunker:    dependencies.Chunker,
		queue:      make(chan api.HookPayload, queueCapacity),
	}
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
	queryVector, err := service.embedder.EmbedQuery(ctx, query)
	if err != nil {
		log.Printf("recall embedding: %v", err)
		writeJSON(writer, map[string]string{})
		return
	}
	hits, err := service.retriever.Search(ctx, retrieval.Query{Text: query, Vector: queryVector, Limit: 4, Project: projectName(payload.CWD)})
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
	var payload api.IngestPayload
	if json.NewDecoder(io.LimitReader(request.Body, 1<<20)).Decode(&payload) != nil || payload.Path == "" {
		http.Error(writer, "path is required", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 30*time.Minute)
	defer cancel()
	documentCount, err := service.IngestPath(ctx, payload)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(writer, map[string]int{"documents": documentCount})
}

func (service *Service) IngestPath(ctx context.Context, request api.IngestPayload) (int, error) {
	request.Project = strings.TrimSpace(request.Project)
	absoluteRoot, err := filepath.Abs(request.Path)
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
		for index, chunk := range service.chunker.Split(body) {
			document := knowledge.Document{Source: "documents", SourceID: fmt.Sprintf("%x-%d", sha256.Sum256([]byte(path)), index), Title: title, Content: chunk, Area: "documents", Scope: knowledge.ScopeGlobal, Metadata: map[string]any{"path": path, "parent": path}}
			if request.Project != "" {
				document.Scope = knowledge.ScopeProject
				document.Project = request.Project
			}
			documents = append(documents, document)
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
		vectors, err := service.embedder.Embed(ctx, texts)
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
			log.Printf("digest %s: processing transcript source=%s", payload.SessionID, payload.Source)
			if err := service.Digest(ctx, payload); err != nil {
				log.Printf("digest %s: %v", payload.SessionID, err)
			} else {
				log.Printf("digest %s: completed in %s", payload.SessionID, time.Since(started).Round(time.Second))
			}
		}
	}
}

func RefreshEmbeddings(ctx context.Context, repository knowledge.Repository, embedder model.Embedder, embeddingVersion string) error {
	if _, err := embedder.Embed(ctx, []string{"Ragfy local knowledge retrieval."}); err != nil {
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
		vectors, err := embedder.Embed(ctx, texts)
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
	info, err := os.Stat(payload.Transcript)
	if err != nil {
		return err
	}
	sessionTimestamp := digest.TranscriptTimestamp(transcript, info.ModTime()).Format(time.RFC3339Nano)
	service.digestLock.Lock()
	defer service.digestLock.Unlock()
	sessionID := payload.SessionID
	if sessionID == "" {
		sessionID = transcriptHash
	}
	alreadyDigested, err := service.repository.HasSession(ctx, sessionID, transcriptHash)
	if err != nil {
		return err
	}
	if alreadyDigested {
		log.Printf("digest %s: skipped unchanged transcript source=%s", sessionID, payload.Source)
		return nil
	}
	spine := digest.TranscriptSpine(transcript)
	if len(spine) < minimumSpineCharacters {
		log.Printf("digest %s: skipped extraction spine_characters=%d minimum=%d stored=0", sessionID, len(spine), minimumSpineCharacters)
		return service.repository.CommitSession(ctx, knowledge.SessionCommit{SessionID: sessionID, TranscriptHash: transcriptHash})
	}
	project := projectName(payload.CWD)
	documents, err := service.extractor.Extract(ctx, extraction.Request{SessionID: sessionID, Source: payload.Source, Project: project, Spine: spine})
	if err != nil {
		return err
	}
	texts := make([]string, len(documents))
	for index, document := range documents {
		texts[index] = document.Title + "\n" + document.Content
	}
	vectors, err := service.embedder.Embed(ctx, texts)
	if err != nil {
		return err
	}
	for index := range documents {
		document := &documents[index]
		document.Vector = vectors[index]
		document.TS = sessionTimestamp
		document.Status = knowledge.StatusActive
		if document.Metadata == nil {
			document.Metadata = make(map[string]any)
		}
		document.Metadata["origin_project"] = project
		document.Metadata["agent_source"] = payload.Source
		document.Metadata["session_id"] = sessionID
		document.Metadata["transcript_hash"] = transcriptHash
		if document.Scope == knowledge.ScopeProject {
			document.Project = project
		} else {
			document.Project = ""
		}
		document.SourceID = SHA256Hex([]byte(sessionID + "\n" + string(document.Scope) + "\n" + document.Project + "\n" + document.Kind + "\n" + document.Content))
	}
	stored, err := service.repository.Documents(ctx)
	if err != nil {
		return err
	}
	candidates := reconciliationCandidates(documents, stored, sessionID)
	actions, err := service.reconciler.Reconcile(ctx, reconciliation.Request{SessionID: sessionID, Source: payload.Source, Project: project, New: documents, Existing: candidates})
	if err != nil {
		return err
	}
	supersessions := resolveSupersessions(actions, documents, candidates)
	if err := service.repository.CommitSession(ctx, knowledge.SessionCommit{SessionID: sessionID, TranscriptHash: transcriptHash, Documents: documents, Supersessions: supersessions}); err != nil {
		return err
	}
	log.Printf("digest %s: stored=%d reconciliation_candidates=%d superseded=%d", sessionID, len(documents), len(candidates), len(supersessions))
	return nil
}

const (
	reconciliationSimilarity        = 0.72
	maximumReconciliationCandidates = 4
)

type scoredDocument struct {
	document knowledge.Document
	score    float64
}

func reconciliationCandidates(newDocuments, stored []knowledge.Document, sessionID string) []knowledge.Document {
	replaced := make(map[string]bool)
	for _, document := range stored {
		if metadataString(document.Metadata, "session_id") == sessionID {
			replaced[document.SourceID] = true
		}
	}
	selected := make(map[string]knowledge.Document)
	for _, incoming := range newDocuments {
		if incoming.Kind == "problem" {
			continue
		}
		var scored []scoredDocument
		for _, existing := range stored {
			if existing.Source != "transcripts" || metadataString(existing.Metadata, "session_id") == sessionID || existing.Kind == "problem" {
				continue
			}
			active := existing.Status == "" || existing.Status == knowledge.StatusActive || replaced[existing.SupersededBy]
			if !active || incoming.Kind != existing.Kind || incoming.Scope != existing.Scope || incoming.Project != existing.Project {
				continue
			}
			score := vectorSimilarity(incoming.Vector, existing.Vector)
			if score >= reconciliationSimilarity {
				scored = append(scored, scoredDocument{document: existing, score: score})
			}
		}
		sort.Slice(scored, func(first, second int) bool { return scored[first].score > scored[second].score })
		for _, candidate := range scored[:min(len(scored), maximumReconciliationCandidates)] {
			selected[candidate.document.SourceID] = candidate.document
		}
	}
	result := make([]knowledge.Document, 0, len(selected))
	for _, document := range selected {
		result = append(result, document)
	}
	sort.Slice(result, func(first, second int) bool { return result[first].SourceID < result[second].SourceID })
	return result
}

func resolveSupersessions(actions []reconciliation.Action, newDocuments, existingDocuments []knowledge.Document) []knowledge.Supersession {
	newByID := documentsBySourceID(newDocuments)
	existingByID := documentsBySourceID(existingDocuments)
	resolved := make(map[string]knowledge.Supersession)
	winnerTimes := make(map[string]time.Time)
	for _, action := range actions {
		incoming, incomingExists := newByID[action.NewSourceID]
		existing, existingExists := existingByID[action.ExistingSourceID]
		if !incomingExists || !existingExists {
			continue
		}
		if incoming.Kind == "problem" || incoming.Kind != existing.Kind || incoming.Scope != existing.Scope || incoming.Project != existing.Project || vectorSimilarity(incoming.Vector, existing.Vector) < reconciliationSimilarity {
			continue
		}
		incomingTime, incomingErr := time.Parse(time.RFC3339Nano, incoming.TS)
		existingTime, existingErr := time.Parse(time.RFC3339Nano, existing.TS)
		loser, winner := existing, incoming
		winnerTime := incomingTime
		if incomingErr != nil || existingErr == nil && !incomingTime.After(existingTime) {
			loser, winner = incoming, existing
			winnerTime = existingTime
		}
		if currentTime, exists := winnerTimes[loser.SourceID]; exists && !winnerTime.After(currentTime) {
			continue
		}
		winnerTimes[loser.SourceID] = winnerTime
		resolved[loser.SourceID] = knowledge.Supersession{LoserSourceID: loser.SourceID, WinnerSourceID: winner.SourceID, Reason: string(action.Relation)}
	}
	result := make([]knowledge.Supersession, 0, len(resolved))
	for _, supersession := range resolved {
		result = append(result, supersession)
	}
	sort.Slice(result, func(first, second int) bool { return result[first].LoserSourceID < result[second].LoserSourceID })
	return result
}

func documentsBySourceID(documents []knowledge.Document) map[string]knowledge.Document {
	result := make(map[string]knowledge.Document, len(documents))
	for _, document := range documents {
		result[document.SourceID] = document
	}
	return result
}

func metadataString(metadata map[string]any, key string) string {
	value, _ := metadata[key].(string)
	return value
}

func vectorSimilarity(first, second []float32) float64 {
	if len(first) == 0 || len(first) != len(second) {
		return 0
	}
	var dot, firstMagnitude, secondMagnitude float64
	for index := range first {
		left, right := float64(first[index]), float64(second[index])
		dot += left * right
		firstMagnitude += left * left
		secondMagnitude += right * right
	}
	if firstMagnitude == 0 || secondMagnitude == 0 {
		return 0
	}
	return dot / (math.Sqrt(firstMagnitude) * math.Sqrt(secondMagnitude))
}

func projectName(workingDirectory string) string {
	project := filepath.Base(filepath.Clean(workingDirectory))
	if project == "." || project == string(filepath.Separator) || project == "" {
		return "unknown"
	}
	return project
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
