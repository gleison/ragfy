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
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"ragfy/internal/api"
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
	queue      chan api.HookPayload
	digestLock sync.Mutex
	startedAt  time.Time
	runtime    RuntimeInfo
}

type RuntimeInfo struct {
	Version string
	Model   string
	Pooling string
}

type Dependencies struct {
	Repository knowledge.Repository
	Embedder   model.Embedder
	Extractor  extraction.Extractor
	Retriever  retrieval.Retriever
	Reconciler reconciliation.Reconciler
	Runtime    RuntimeInfo
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
		queue:      make(chan api.HookPayload, queueCapacity),
		startedAt:  time.Now(),
		runtime:    dependencies.Runtime,
	}
}

func (service *Service) Routes(token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", authenticate(token, func(writer http.ResponseWriter, request *http.Request) {
		statistics, err := service.repository.Statistics(request.Context())
		if err != nil {
			log.Printf("health statistics: %v", err)
		}
		writeJSON(writer, map[string]any{
			"ok":         true,
			"queued":     len(service.queue),
			"started_at": service.startedAt.UTC().Format(time.RFC3339),
			"version":    service.runtime.Version,
			"model":      service.runtime.Model,
			"pooling":    service.runtime.Pooling,
			"knowledge":  statistics,
		})
	}))
	mux.HandleFunc("POST /recall", authenticate(token, service.handleRecall))
	mux.HandleFunc("POST /digest", authenticate(token, service.handleDigest))
	mux.HandleFunc("POST /ingest", authenticate(token, service.handleIngest))
	mux.HandleFunc("GET /sessions", authenticate(token, service.handleSessions))
	mux.HandleFunc("GET /sessions/{id}", authenticate(token, service.handleSession))
	mux.HandleFunc("DELETE /sessions/{id}", authenticate(token, service.handleForgetSession))
	mux.HandleFunc("GET /recalls", authenticate(token, service.handleRecalls))
	mux.HandleFunc("GET /recalls/{id}", authenticate(token, service.handleRecallEvent))
	mux.HandleFunc("PUT /recalls/{id}/results/{rank}/feedback", authenticate(token, service.handleRecallFeedback))
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
	if len(query) < 12 && payload.Source != "cli" {
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
	project := strings.TrimSpace(payload.Project)
	if project == "" {
		project = projectName(payload.CWD)
	}
	explanation, err := service.retriever.Explain(ctx, retrieval.Query{Text: query, Vector: queryVector, Limit: 4, Project: project})
	if err != nil {
		log.Printf("recall search: %v", err)
		writeJSON(writer, map[string]string{})
		return
	}
	hits := explanation.Selected
	var contextText strings.Builder
	for index, hit := range hits {
		fmt.Fprintf(&contextText, "[%d] %s\n%s\n\n", index+1, hit.Doc.Title, hit.Doc.Content)
	}
	if payload.Source != "cli" && strings.TrimSpace(payload.SessionID) != "" {
		now := time.Now().UTC()
		event := knowledge.RecallEvent{
			ID:            SHA256Hex([]byte(payload.SessionID + "\x00" + query + "\x00" + now.Format(time.RFC3339Nano))),
			SessionID:     payload.SessionID,
			Source:        payload.Source,
			Project:       project,
			Prompt:        query,
			CreatedAt:     now.Format(time.RFC3339Nano),
			ScoreCutoff:   explanation.ScoreCutoff,
			LexicalCutoff: explanation.LexicalCutoff,
		}
		for _, candidate := range explanation.Candidates {
			hit := candidate.Hit
			event.Results = append(event.Results, knowledge.RecallResult{
				Rank: candidate.Rank, SelectedRank: candidate.SelectedRank, Status: candidate.Status,
				SourceID: hit.Doc.SourceID, Title: hit.Doc.Title, Content: hit.Doc.Content,
				Kind: hit.Doc.Kind, Scope: hit.Doc.Scope, Project: hit.Doc.Project, Score: hit.Score,
				SemanticScore: hit.SemanticScore, LexicalScore: hit.LexicalScore, ProjectBoost: hit.ProjectBoost,
			})
		}
		event.ResultCount = len(hits)
		event.CandidateCount = len(event.Results)
		if err := service.repository.RecordRecall(request.Context(), event); err != nil {
			log.Printf("recall audit session=%s: %v", payload.SessionID, err)
		}
	}
	if contextText.Len() > 0 && payload.Source != "cli" {
		contextText.WriteString("\n---\nKB RECALL: Retrieved from your local knowledge base. Use only when relevant.")
	}
	writeJSON(writer, api.RecallResponse{Context: contextText.String()})
}

func (service *Service) handleSessions(writer http.ResponseWriter, request *http.Request) {
	sessions, err := service.repository.Sessions(request.Context())
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(writer, sessions)
}

func (service *Service) handleSession(writer http.ResponseWriter, request *http.Request) {
	session, err := service.repository.Session(request.Context(), request.PathValue("id"))
	if errors.Is(err, knowledge.ErrNotFound) {
		http.Error(writer, "session not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(writer, session)
}

func (service *Service) handleForgetSession(writer http.ResponseWriter, request *http.Request) {
	result, err := service.repository.ForgetSession(request.Context(), request.PathValue("id"))
	if errors.Is(err, knowledge.ErrNotFound) {
		http.Error(writer, "session not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(writer, result)
}

func (service *Service) handleRecalls(writer http.ResponseWriter, request *http.Request) {
	events, err := service.repository.RecallEvents(request.Context())
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(writer, events)
}

func (service *Service) handleRecallEvent(writer http.ResponseWriter, request *http.Request) {
	event, err := service.repository.RecallEvent(request.Context(), request.PathValue("id"))
	if errors.Is(err, knowledge.ErrNotFound) {
		http.Error(writer, "recall not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(writer, event)
}

func (service *Service) handleRecallFeedback(writer http.ResponseWriter, request *http.Request) {
	rank, err := strconv.Atoi(request.PathValue("rank"))
	if err != nil || rank <= 0 {
		http.Error(writer, "invalid candidate rank", http.StatusBadRequest)
		return
	}
	var payload api.RecallFeedbackPayload
	if json.NewDecoder(io.LimitReader(request.Body, 1<<20)).Decode(&payload) != nil || payload.Feedback != "useful" && payload.Feedback != "irrelevant" {
		http.Error(writer, "feedback must be useful or irrelevant", http.StatusBadRequest)
		return
	}
	result, err := service.repository.SetRecallFeedback(request.Context(), request.PathValue("id"), rank, payload.Feedback)
	if errors.Is(err, knowledge.ErrNotFound) {
		http.Error(writer, "selected recall result not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(writer, result)
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
	if json.NewDecoder(io.LimitReader(request.Body, 1<<20)).Decode(&payload) != nil {
		http.Error(writer, "invalid payload", http.StatusBadRequest)
		return
	}
	hasPath := strings.TrimSpace(payload.Path) != ""
	hasText := strings.TrimSpace(payload.Text) != ""
	if hasPath == hasText {
		http.Error(writer, "provide either path or text", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 30*time.Minute)
	defer cancel()
	documentCount := 0
	var err error
	if hasText {
		documentCount, err = service.IngestText(ctx, payload)
	} else {
		documentCount, err = service.IngestPath(ctx, payload)
	}
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
	provider, err := service.resolveProvider(request.Provider)
	if err != nil {
		return 0, err
	}
	storedCount := 0
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
		source := newKnowledgeSource("file", path, provider, request.Project, title, body, info.ModTime())
		count, err := service.processSource(ctx, source)
		storedCount += count
		return err
	})
	if err != nil {
		return storedCount, err
	}
	return storedCount, nil
}

func (service *Service) IngestText(ctx context.Context, request api.IngestPayload) (int, error) {
	text := strings.TrimSpace(request.Text)
	if text == "" {
		return 0, errors.New("text is required")
	}
	project := strings.TrimSpace(request.Project)
	title := strings.TrimSpace(request.Title)
	if title == "" {
		title = manualMemoryTitle(text)
	}
	provider, err := service.resolveProvider(request.Provider)
	if err != nil {
		return 0, err
	}
	key := title
	if strings.TrimSpace(request.Title) == "" {
		key = SHA256Hex([]byte(text))
	}
	return service.processSource(ctx, newKnowledgeSource("manual", key, provider, project, title, text, time.Now()))
}

func manualMemoryTitle(text string) string {
	firstLine, _, _ := strings.Cut(text, "\n")
	firstLine = strings.TrimSpace(firstLine)
	if utf8.RuneCountInString(firstLine) <= 80 {
		return firstLine
	}
	runes := []rune(firstLine)
	return strings.TrimSpace(string(runes[:77])) + "..."
}

func newKnowledgeSource(kind, key, provider, project, title, content string, createdAt time.Time) knowledge.Source {
	contentHash := SHA256Hex([]byte(content))
	return knowledge.Source{
		ID:          SHA256Hex([]byte(kind + "\x00" + key + "\x00" + provider + "\x00" + project + "\x00" + contentHash)),
		Kind:        kind,
		Key:         key,
		Provider:    provider,
		Project:     project,
		Title:       title,
		Content:     content,
		ContentHash: contentHash,
		CreatedAt:   createdAt.UTC().Format(time.RFC3339Nano),
	}
}

type providerResolver interface {
	ResolveProvider(string) (string, error)
}

func (service *Service) resolveProvider(requested string) (string, error) {
	requested = strings.ToLower(strings.TrimSpace(requested))
	if resolver, ok := service.extractor.(providerResolver); ok {
		return resolver.ResolveProvider(requested)
	}
	if requested == "" {
		return "codex", nil
	}
	return requested, nil
}

func (service *Service) processSource(ctx context.Context, source knowledge.Source) (int, error) {
	alreadyProcessed, err := service.repository.HasSource(ctx, source.ID)
	if err != nil || alreadyProcessed {
		return 0, err
	}
	documents, err := service.extractor.Extract(ctx, extraction.Request{SessionID: source.ID, Provider: source.Provider, Project: source.Project, Spine: source.Content, Kind: source.Kind, Title: source.Title})
	if err != nil {
		return 0, err
	}
	if err := service.prepareDocuments(ctx, documents, source); err != nil {
		return 0, err
	}
	stored, err := service.repository.Documents(ctx)
	if err != nil {
		return 0, err
	}
	candidates := reconciliationCandidates(documents, stored, source)
	actions, err := service.reconciler.Reconcile(ctx, reconciliation.Request{SessionID: source.ID, Provider: source.Provider, Project: source.Project, New: documents, Existing: candidates})
	if err != nil {
		return 0, err
	}
	supersessions := resolveSupersessions(actions, documents, candidates)
	if err := service.repository.CommitSource(ctx, knowledge.SourceCommit{Source: source, Documents: documents, Supersessions: supersessions}); err != nil {
		return 0, err
	}
	log.Printf("ingest %s: source=%s stored=%d reconciliation_candidates=%d superseded=%d", source.ID, source.Kind, len(documents), len(candidates), len(supersessions))
	return len(documents), nil
}

func (service *Service) prepareDocuments(ctx context.Context, documents []knowledge.Document, source knowledge.Source) error {
	if len(documents) == 0 {
		return nil
	}
	texts := make([]string, len(documents))
	for index, document := range documents {
		texts[index] = document.Title + "\n" + document.Content
	}
	vectors, err := service.embedder.Embed(ctx, texts)
	if err != nil {
		return err
	}
	if len(vectors) != len(documents) {
		return fmt.Errorf("expected %d embeddings, got %d", len(documents), len(vectors))
	}
	for index := range documents {
		document := &documents[index]
		document.Vector = vectors[index]
		document.TS = source.CreatedAt
		document.Status = knowledge.StatusActive
		document.OriginID = source.ID
		if document.Metadata == nil {
			document.Metadata = make(map[string]any)
		}
		document.Metadata["origin_kind"] = source.Kind
		document.Metadata["origin_project"] = source.Project
		document.Metadata["source_key"] = source.Key
		document.Metadata["content_hash"] = source.ContentHash
		document.Metadata["agent_source"] = source.Provider
		if source.Kind == "file" {
			document.Metadata["path"] = source.Key
		}
		if source.Kind == "manual" {
			document.Metadata["origin"] = "cli"
		}
		if source.Kind == "session" {
			document.Metadata["session_id"] = source.Key
			document.Metadata["transcript_hash"] = source.ContentHash
		}
		if document.Scope == knowledge.ScopeProject {
			document.Project = source.Project
		} else {
			document.Project = ""
		}
		document.SourceID = SHA256Hex([]byte(source.ID + "\n" + string(document.Scope) + "\n" + document.Project + "\n" + document.Kind + "\n" + document.Content))
	}
	return nil
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
	sessionTimestamp := digest.TranscriptTimestamp(transcript, info.ModTime())
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
	project := projectName(payload.CWD)
	source := newKnowledgeSource("session", sessionID, payload.Source, project, "Session "+sessionID, spine, sessionTimestamp)
	if len(spine) < minimumSpineCharacters {
		log.Printf("digest %s: skipped extraction spine_characters=%d minimum=%d stored=0", sessionID, len(spine), minimumSpineCharacters)
		return service.repository.CommitSession(ctx, knowledge.SessionCommit{SessionID: sessionID, TranscriptHash: transcriptHash, Source: source})
	}
	documents, err := service.extractor.Extract(ctx, extraction.Request{SessionID: sessionID, Provider: payload.Source, Project: project, Spine: spine})
	if err != nil {
		return err
	}
	if err := service.prepareDocuments(ctx, documents, source); err != nil {
		return err
	}
	for index := range documents {
		documents[index].Metadata["transcript_hash"] = transcriptHash
	}
	stored, err := service.repository.Documents(ctx)
	if err != nil {
		return err
	}
	candidates := reconciliationCandidates(documents, stored, source)
	actions, err := service.reconciler.Reconcile(ctx, reconciliation.Request{SessionID: sessionID, Provider: payload.Source, Project: project, New: documents, Existing: candidates})
	if err != nil {
		return err
	}
	supersessions := resolveSupersessions(actions, documents, candidates)
	if err := service.repository.CommitSession(ctx, knowledge.SessionCommit{SessionID: sessionID, TranscriptHash: transcriptHash, Source: source, Documents: documents, Supersessions: supersessions}); err != nil {
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

func reconciliationCandidates(newDocuments, stored []knowledge.Document, source knowledge.Source) []knowledge.Document {
	replaced := make(map[string]bool)
	for _, document := range stored {
		if sameSourceLineage(document, source) {
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
			if existing.Source != "transcripts" || sameSourceLineage(existing, source) || existing.Kind == "problem" {
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

func sameSourceLineage(document knowledge.Document, source knowledge.Source) bool {
	key := metadataString(document.Metadata, "source_key")
	if key == "" {
		sessionID := metadataString(document.Metadata, "session_id")
		if source.Kind == "session" && sessionID == source.Key {
			return true
		}
		key = sessionID
	}
	return key == source.Key && metadataString(document.Metadata, "origin_project") == source.Project
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
