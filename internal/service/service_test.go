package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"ragfy/internal/api"
	"ragfy/internal/digest"
	"ragfy/internal/extraction"
	"ragfy/internal/knowledge"
	"ragfy/internal/reconciliation"
	"ragfy/internal/retrieval"
	"ragfy/internal/storage/sqlite"
)

const testEmbeddingVersion = "test-model-v1"

type fakeModelEngine struct {
	mu            sync.Mutex
	generated     []string
	generateCalls int
	embedCalls    int
	generateErr   error
	embedErr      error
}

type fakeReconciler struct {
	requests []reconciliation.Request
	relation reconciliation.Relation
	err      error
}

func (f *fakeReconciler) Reconcile(_ context.Context, request reconciliation.Request) ([]reconciliation.Action, error) {
	f.requests = append(f.requests, request)
	if f.err != nil || len(request.New) == 0 || len(request.Existing) == 0 {
		return nil, f.err
	}
	if f.relation == "" {
		return nil, nil
	}
	return []reconciliation.Action{{NewSourceID: request.New[0].SourceID, ExistingSourceID: request.Existing[0].SourceID, Relation: f.relation}}, nil
}

func (f *fakeModelEngine) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.embedCalls++
	if f.embedErr != nil {
		return nil, f.embedErr
	}
	vectors := make([][]float32, len(texts))
	for i := range texts {
		vectors[i] = []float32{1, 0}
	}
	return vectors, nil
}

func (f *fakeModelEngine) EmbedQuery(ctx context.Context, query string) ([]float32, error) {
	vectors, err := f.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}

func (f *fakeModelEngine) Generate(_ context.Context, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := f.generateCalls
	f.generateCalls++
	if f.generateErr != nil {
		return "", f.generateErr
	}
	if call >= len(f.generated) {
		return "", fmt.Errorf("unexpected Generate call %d", call+1)
	}
	return f.generated[call], nil
}

func (f *fakeModelEngine) Close() error { return nil }

func openTestRepository(t *testing.T) *sqlite.Repository {
	t.Helper()
	repository, err := sqlite.Open(sqlite.Config{Path: filepath.Join(t.TempDir(), "memory.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	return repository
}

func newTestService(repository knowledge.Repository, corpus retrieval.Corpus, models *fakeModelEngine, queueCapacity int) *Service {
	return NewWithQueueCapacity(Dependencies{
		Repository: repository,
		Embedder:   models,
		Extractor:  extraction.NewGenerative(models),
		Retriever:  retrieval.NewHybrid(corpus),
	}, queueCapacity)
}

func requestJSON(t *testing.T, handler http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &payload)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func TestServiceRoutesAuthenticateAndRecallKnowledge(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepository(t)
	if err := repo.Upsert(ctx, knowledge.Document{
		Source: "transcripts", SourceID: "persistent", Title: "Persistent daemon",
		Content: "Prompt hooks call one resident Go service.", Scope: knowledge.ScopeGlobal, Vector: []float32{1, 0},
	}); err != nil {
		t.Fatal(err)
	}
	models := &fakeModelEngine{}
	service := newTestService(repo, repo, models, 1)
	handler := service.Routes("secret")

	if got := requestJSON(t, handler, http.MethodGet, "/health", "wrong", nil); got.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized health status = %d", got.Code)
	}
	health := requestJSON(t, handler, http.MethodGet, "/health", "secret", nil)
	if health.Code != http.StatusOK || !strings.Contains(health.Body.String(), `"ok":true`) || !strings.Contains(health.Body.String(), `"knowledge"`) {
		t.Fatalf("health response = %d %s", health.Code, health.Body.String())
	}

	recall := requestJSON(t, handler, http.MethodPost, "/recall", "secret", api.HookPayload{Prompt: "How do prompt hooks avoid model reloads?"})
	if recall.Code != http.StatusOK {
		t.Fatalf("recall status = %d: %s", recall.Code, recall.Body.String())
	}
	var response map[string]string
	if err := json.Unmarshal(recall.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response["context"], "Persistent daemon") || !strings.Contains(response["context"], "KB RECALL") {
		t.Fatalf("unexpected recall context: %q", response["context"])
	}
	if models.embedCalls != 1 {
		t.Fatalf("embed calls = %d, want 1", models.embedCalls)
	}

	short := requestJSON(t, handler, http.MethodPost, "/recall", "secret", api.HookPayload{Prompt: "short"})
	if short.Code != http.StatusOK || short.Body.String() != "{}\n" {
		t.Fatalf("short prompt response = %d %q", short.Code, short.Body.String())
	}
}

func TestSessionBootstrapLoadsOnlyBoundedActiveProjectContext(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepository(t)
	documents := []knowledge.Document{
		{Source: "sessions", SourceID: "older", TS: "2026-10-01T00:00:00Z", Title: "Older project decision", Content: "older content", Kind: "decision", Scope: knowledge.ScopeProject, Project: "ragfy"},
		{Source: "sessions", SourceID: "newer", TS: "2026-11-01T00:00:00Z", Title: "Newer project decision", Content: "newer content", Kind: "decision", Scope: knowledge.ScopeProject, Project: "ragfy"},
		{Source: "sessions", SourceID: "other", TS: "2026-11-02T00:00:00Z", Title: "Other project", Content: "must not appear", Kind: "decision", Scope: knowledge.ScopeProject, Project: "other"},
		{Source: "manual", SourceID: "global", TS: "2026-11-03T00:00:00Z", Title: "Global knowledge", Content: "must not appear", Kind: "fact", Scope: knowledge.ScopeGlobal},
		{Source: "sessions", SourceID: "retired", TS: "2026-11-04T00:00:00Z", Title: "Retired project fact", Content: "must not appear", Kind: "fact", Scope: knowledge.ScopeProject, Project: "ragfy", Status: knowledge.StatusSuperseded},
	}
	for _, document := range documents {
		if err := repo.Upsert(ctx, document); err != nil {
			t.Fatal(err)
		}
	}
	models := &fakeModelEngine{}
	handler := newTestService(repo, repo, models, 1).Routes("secret")
	bootstrap := requestJSON(t, handler, http.MethodPost, "/bootstrap", "secret", api.HookPayload{Source: "codex", SessionID: "session", CWD: "/workspace/ragfy"})
	if bootstrap.Code != http.StatusOK {
		t.Fatalf("bootstrap status = %d: %s", bootstrap.Code, bootstrap.Body.String())
	}
	var response api.RecallResponse
	if err := json.Unmarshal(bootstrap.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response.Context, "Newer project decision") || !strings.Contains(response.Context, "Older project decision") || !strings.Contains(response.Context, "KB PROJECT CONTEXT") {
		t.Fatalf("bootstrap context = %q", response.Context)
	}
	if strings.Index(response.Context, "Newer project decision") > strings.Index(response.Context, "Older project decision") {
		t.Fatalf("bootstrap context is not newest first: %q", response.Context)
	}
	for _, excluded := range []string{"Other project", "Global knowledge", "Retired project fact", "must not appear"} {
		if strings.Contains(response.Context, excluded) {
			t.Fatalf("bootstrap included %q: %q", excluded, response.Context)
		}
	}
	if models.embedCalls != 0 {
		t.Fatalf("bootstrap loaded the embedding model %d times", models.embedCalls)
	}
}

func TestSessionBootstrapBoundsContextLength(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepository(t)
	if err := repo.Upsert(ctx, knowledge.Document{
		Source: "manual", SourceID: "large", TS: "2026-11-01T00:00:00Z", Title: "Large project note",
		Content: strings.Repeat("á", maximumBootstrapContentLength), Kind: "fact", Scope: knowledge.ScopeProject, Project: "ragfy",
	}); err != nil {
		t.Fatal(err)
	}
	handler := newTestService(repo, repo, &fakeModelEngine{}, 1).Routes("secret")
	bootstrap := requestJSON(t, handler, http.MethodPost, "/bootstrap", "secret", api.HookPayload{CWD: "/workspace/ragfy"})
	var response api.RecallResponse
	if bootstrap.Code != http.StatusOK || json.Unmarshal(bootstrap.Body.Bytes(), &response) != nil {
		t.Fatalf("bootstrap=%d %s", bootstrap.Code, bootstrap.Body.String())
	}
	if length := utf8.RuneCountInString(response.Context); length != maximumBootstrapContentLength {
		t.Fatalf("bootstrap context length = %d, want %d", length, maximumBootstrapContentLength)
	}
	if !strings.HasSuffix(response.Context, bootstrapContextFooter) {
		t.Fatalf("bootstrap context is missing footer: %q", response.Context[len(response.Context)-200:])
	}
}

func TestRecallAuditAndSessionRoutes(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepository(t)
	source := knowledge.Source{ID: "session-source", Kind: "session", Key: "session-one", Provider: "codex", Project: "ragfy", Title: "Session session-one", Content: "conversation", ContentHash: "hash", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	document := knowledge.Document{Source: "transcripts", SourceID: "remembered", Title: "Remembered fact", Content: "Ragfy records selected recall results.", Kind: "fact", Scope: knowledge.ScopeProject, Project: "ragfy", Vector: []float32{1, 0}}
	if err := repo.CommitSession(ctx, knowledge.SessionCommit{SessionID: "session-one", TranscriptHash: "hash", Source: source, Documents: []knowledge.Document{document}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Upsert(ctx, knowledge.Document{Source: "manual", SourceID: "tangential", Title: "Tangential note", Content: "An unrelated operational note.", Kind: "fact", Scope: knowledge.ScopeGlobal, Vector: []float32{0.6, 0.8}}); err != nil {
		t.Fatal(err)
	}
	service := newTestService(repo, repo, &fakeModelEngine{}, 1)
	handler := service.Routes("secret")
	recall := requestJSON(t, handler, http.MethodPost, "/recall", "secret", api.HookPayload{Source: "codex", SessionID: "session-one", Prompt: "How are selected recall results recorded?", CWD: "/workspace/ragfy"})
	if recall.Code != http.StatusOK || !strings.Contains(recall.Body.String(), "Remembered fact") {
		t.Fatalf("recall=%d %s", recall.Code, recall.Body.String())
	}
	list := requestJSON(t, handler, http.MethodGet, "/recalls", "secret", nil)
	var events []knowledge.RecallEvent
	if list.Code != http.StatusOK || json.Unmarshal(list.Body.Bytes(), &events) != nil || len(events) != 1 || events[0].SessionID != "session-one" || events[0].ResultCount != 1 || events[0].CandidateCount != 2 {
		t.Fatalf("recalls=%d %s", list.Code, list.Body.String())
	}
	detail := requestJSON(t, handler, http.MethodGet, "/recalls/"+events[0].ID, "secret", nil)
	var event knowledge.RecallEvent
	if detail.Code != http.StatusOK || json.Unmarshal(detail.Body.Bytes(), &event) != nil || len(event.Results) != 2 || event.Results[0].SourceID != "remembered" || event.Results[0].Status != "selected" || event.Results[1].Status != "cutoff" {
		t.Fatalf("recall detail=%d %s", detail.Code, detail.Body.String())
	}
	feedback := requestJSON(t, handler, http.MethodPut, "/recalls/"+events[0].ID+"/results/1/feedback", "secret", api.RecallFeedbackPayload{Feedback: "useful"})
	if feedback.Code != http.StatusOK || !strings.Contains(feedback.Body.String(), `"feedback":"useful"`) {
		t.Fatalf("feedback=%d %s", feedback.Code, feedback.Body.String())
	}
	cutFeedback := requestJSON(t, handler, http.MethodPut, "/recalls/"+events[0].ID+"/results/2/feedback", "secret", api.RecallFeedbackPayload{Feedback: "irrelevant"})
	if cutFeedback.Code != http.StatusNotFound {
		t.Fatalf("cut feedback=%d %s", cutFeedback.Code, cutFeedback.Body.String())
	}
	sessions := requestJSON(t, handler, http.MethodGet, "/sessions", "secret", nil)
	if sessions.Code != http.StatusOK || !strings.Contains(sessions.Body.String(), "session-one") {
		t.Fatalf("sessions=%d %s", sessions.Code, sessions.Body.String())
	}
	session := requestJSON(t, handler, http.MethodGet, "/sessions/session-one", "secret", nil)
	if session.Code != http.StatusOK || !strings.Contains(session.Body.String(), "remembered") {
		t.Fatalf("session=%d %s", session.Code, session.Body.String())
	}
	forgotten := requestJSON(t, handler, http.MethodDelete, "/sessions/session-one", "secret", nil)
	if forgotten.Code != http.StatusOK || !strings.Contains(forgotten.Body.String(), `"removed_records":1`) || !strings.Contains(forgotten.Body.String(), `"removed_recalls":1`) {
		t.Fatalf("forget=%d %s", forgotten.Code, forgotten.Body.String())
	}
	missing := requestJSON(t, handler, http.MethodGet, "/sessions/session-one", "secret", nil)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing session status=%d body=%s", missing.Code, missing.Body.String())
	}
}

func TestRecallUsesGlobalAndCurrentProjectKnowledge(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepository(t)
	for _, document := range []knowledge.Document{
		{Source: "transcripts", SourceID: "global", Title: "Global convention", Content: "Use meaningful names in every codebase.", Scope: knowledge.ScopeGlobal, Vector: []float32{1, 0}},
		{Source: "transcripts", SourceID: "ragfy", Title: "Ragfy design", Content: "Ragfy uses a resident service.", Scope: knowledge.ScopeProject, Project: "ragfy", Vector: []float32{1, 0}},
		{Source: "transcripts", SourceID: "other", Title: "Other design", Content: "Other uses a remote service.", Scope: knowledge.ScopeProject, Project: "other", Vector: []float32{1, 0}},
	} {
		if err := repo.Upsert(ctx, document); err != nil {
			t.Fatal(err)
		}
	}
	service := newTestService(repo, repo, &fakeModelEngine{}, 1)
	response := requestJSON(t, service.Routes("secret"), http.MethodPost, "/recall", "secret", api.HookPayload{Prompt: "What service and naming conventions should I use?", CWD: "/workspace/ragfy"})
	var body map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body["context"], "Global convention") || !strings.Contains(body["context"], "Ragfy design") || strings.Contains(body["context"], "Other design") {
		t.Fatalf("unexpected scoped recall: %q", body["context"])
	}
}

func TestCLISearchAcceptsShortQueriesWithoutHookFooter(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepository(t)
	if err := repo.Upsert(ctx, knowledge.Document{Source: "manual", SourceID: "cache", Title: "Go cache", Content: "Use a temporary GOCACHE in restricted environments.", Scope: knowledge.ScopeGlobal, Vector: []float32{1, 0}}); err != nil {
		t.Fatal(err)
	}
	service := newTestService(repo, repo, &fakeModelEngine{}, 1)
	response := requestJSON(t, service.Routes("secret"), http.MethodPost, "/recall", "secret", api.HookPayload{Source: "cli", Prompt: "Go cache"})
	var body api.RecallResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body.Context, "Go cache") || strings.Contains(body.Context, "KB RECALL") {
		t.Fatalf("CLI recall context = %q", body.Context)
	}
}

func TestDigestEndpointQueuesWithoutWaitingAndReportsFullQueue(t *testing.T) {
	service := NewWithQueueCapacity(Dependencies{}, 1)
	handler := service.Routes("secret")
	payload := api.HookPayload{SessionID: "session", Transcript: "/tmp/transcript.jsonl"}
	first := requestJSON(t, handler, http.MethodPost, "/digest", "secret", payload)
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), `"queued":true`) {
		t.Fatalf("first enqueue = %d %s", first.Code, first.Body.String())
	}
	second := requestJSON(t, handler, http.MethodPost, "/digest", "secret", payload)
	if second.Code != http.StatusServiceUnavailable || !strings.Contains(second.Body.String(), "queue full") {
		t.Fatalf("full queue response = %d %s", second.Code, second.Body.String())
	}
}

func writeCodexTranscript(t *testing.T, path string, messages ...string) {
	writeCodexTranscriptAt(t, path, "", messages...)
}

func writeCodexTranscriptAt(t *testing.T, path, timestamp string, messages ...string) {
	t.Helper()
	var lines []string
	for _, message := range messages {
		row := map[string]any{"type": "event_msg", "payload": map[string]any{"type": "user_message", "message": message}}
		if timestamp != "" {
			row["timestamp"] = timestamp
		}
		encoded, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(encoded))
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func digestFactAt(t *testing.T, service *Service, path, sessionID, project, timestamp, evidence string) {
	t.Helper()
	writeCodexTranscriptAt(t, path, timestamp, evidence, strings.Repeat("Durable implementation context. ", 25))
	if err := service.Digest(context.Background(), api.HookPayload{SessionID: sessionID, Source: "codex", Transcript: path, CWD: "/workspace/" + project}); err != nil {
		t.Fatal(err)
	}
}

func groundedFactJSON(fact, evidence string) string {
	return fmt.Sprintf(`{"problems":[],"facts":[{"fact":%q,"area":"runtime","status":"current","evidence":%q,"visibility":"project"}],"preferences":[],"decisions":[]}`, fact, evidence)
}

func groundedGlobalFactJSON(fact, evidence string) string {
	return fmt.Sprintf(`{"problems":[],"facts":[{"fact":%q,"area":"runtime","status":"current","evidence":%q,"visibility":"global"}],"preferences":[],"decisions":[]}`, fact, evidence)
}

func TestDigestEndToEndDeduplicatesAndReplacesSessionKnowledge(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepository(t)
	firstEvidence := "Ragfy runs as a persistent local Go daemon for prompt hooks."
	secondEvidence := "Ragfy stores its local knowledge in a SQLite repository."
	models := &fakeModelEngine{generated: []string{
		groundedFactJSON(firstEvidence, firstEvidence),
		groundedFactJSON(secondEvidence, secondEvidence),
	}}
	service := newTestService(repo, repo, models, 1)
	transcript := filepath.Join(t.TempDir(), "session.jsonl")
	filler := strings.Repeat("The implementation discussion contains durable technical context. ", 12)
	writeCodexTranscript(t, transcript, firstEvidence, filler)
	payload := api.HookPayload{SessionID: "session-1", Source: "codex", Transcript: transcript, CWD: "/workspace/ragfy"}

	if err := service.Digest(ctx, payload); err != nil {
		t.Fatal(err)
	}
	if err := service.Digest(ctx, payload); err != nil {
		t.Fatal(err)
	}
	if models.generateCalls != 1 {
		t.Fatalf("unchanged transcript generated %d times, want 1", models.generateCalls)
	}

	writeCodexTranscript(t, transcript, firstEvidence, filler, secondEvidence)
	if err := service.Digest(ctx, payload); err != nil {
		t.Fatal(err)
	}
	docs, err := repo.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 || docs[0].Status != knowledge.StatusSuperseded || docs[1].Status != knowledge.StatusActive || !strings.Contains(docs[1].Content, "SQLite repository") {
		t.Fatalf("session knowledge was not replaced: %#v", docs)
	}
	if docs[1].Scope != knowledge.ScopeProject || docs[1].Project != "ragfy" || docs[1].Metadata["session_id"] != "session-1" || docs[1].Metadata["origin_project"] != "ragfy" || docs[1].Metadata["agent_source"] != "codex" || docs[1].Metadata["evidence"] != secondEvidence {
		t.Fatalf("missing digest metadata: %#v", docs[1].Metadata)
	}
	sources, err := repo.Sources(ctx)
	if err != nil || len(sources) != 2 || sources[0].Kind != "session" || !strings.Contains(sources[1].Content, secondEvidence) {
		t.Fatalf("session sources were not preserved: %#v err=%v", sources, err)
	}
}

func TestDigestStoresGlobalKnowledgeWithoutProjectRestriction(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepository(t)
	evidence := "Sempre use métodos atômicos e nomes significativos em qualquer projeto."
	response := fmt.Sprintf(`{"problems":[],"facts":[],"preferences":[{"preference":"Usar métodos atômicos e nomes significativos.","why":"","applies_to":"qualquer projeto","evidence":%q,"acceptance":"explicit","acceptance_evidence":"","visibility":"global"}],"decisions":[]}`, evidence)
	service := newTestService(repo, repo, &fakeModelEngine{generated: []string{response}}, 1)
	transcript := filepath.Join(t.TempDir(), "session.jsonl")
	writeCodexTranscript(t, transcript, evidence, strings.Repeat("Additional durable engineering context. ", 20))
	if err := service.Digest(ctx, api.HookPayload{SessionID: "global", Source: "codex", Transcript: transcript, CWD: "/workspace/ragfy"}); err != nil {
		t.Fatal(err)
	}
	documents, err := repo.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 1 || documents[0].Scope != knowledge.ScopeGlobal || documents[0].Project != "" || documents[0].Metadata["origin_project"] != "ragfy" {
		t.Fatalf("documents=%#v", documents)
	}
}

func TestDigestSupersedesOlderKnowledgeAcrossSessions(t *testing.T) {
	repo := openTestRepository(t)
	oldFact := "Ragfy stores durable knowledge in an embedded file database."
	newFact := "Ragfy stores durable knowledge in the PostgreSQL service."
	models := &fakeModelEngine{generated: []string{groundedFactJSON(oldFact, oldFact), groundedFactJSON(newFact, newFact)}}
	reconciler := &fakeReconciler{relation: reconciliation.RelationSupersedes}
	service := newTestService(repo, repo, models, 1)
	service.reconciler = reconciler
	directory := t.TempDir()
	digestFactAt(t, service, filepath.Join(directory, "old.jsonl"), "old", "ragfy", "2026-01-02T12:00:00Z", oldFact)
	digestFactAt(t, service, filepath.Join(directory, "new.jsonl"), "new", "ragfy", "2026-02-02T12:00:00Z", newFact)

	documents, err := repo.Documents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 2 {
		t.Fatalf("documents=%#v", documents)
	}
	var active, superseded knowledge.Document
	for _, document := range documents {
		if document.Status == knowledge.StatusActive {
			active = document
		} else {
			superseded = document
		}
	}
	if !strings.Contains(active.Content, "PostgreSQL") || superseded.Status != knowledge.StatusSuperseded || superseded.SupersededBy != active.SourceID || superseded.ConsolidationReason != "supersedes" {
		t.Fatalf("unexpected consolidation: active=%#v superseded=%#v", active, superseded)
	}
	hits, err := retrieval.NewHybrid(repo).Search(context.Background(), retrieval.Query{Text: "Where does Ragfy store durable knowledge?", Vector: []float32{1, 0}, Limit: 4, Project: "ragfy"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Doc.SourceID != active.SourceID {
		t.Fatalf("superseded knowledge was searchable: %#v", hits)
	}
}

func TestDigestDoesNotLetLateProcessedOldSessionWin(t *testing.T) {
	repo := openTestRepository(t)
	newFact := "Ragfy stores durable knowledge in the PostgreSQL service."
	oldFact := "Ragfy stores durable knowledge in an embedded file database."
	models := &fakeModelEngine{generated: []string{groundedFactJSON(newFact, newFact), groundedFactJSON(oldFact, oldFact)}}
	reconciler := &fakeReconciler{relation: reconciliation.RelationSupersedes}
	service := newTestService(repo, repo, models, 1)
	service.reconciler = reconciler
	directory := t.TempDir()
	digestFactAt(t, service, filepath.Join(directory, "new.jsonl"), "new", "ragfy", "2026-02-02T12:00:00Z", newFact)
	digestFactAt(t, service, filepath.Join(directory, "old.jsonl"), "old", "ragfy", "2026-01-02T12:00:00Z", oldFact)

	documents, err := repo.Documents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, document := range documents {
		if strings.Contains(document.Content, "PostgreSQL") && document.Status != knowledge.StatusActive {
			t.Fatalf("newer knowledge was superseded: %#v", documents)
		}
		if strings.Contains(document.Content, "embedded file") && (document.Status != knowledge.StatusSuperseded || document.ConsolidationReason != "supersedes") {
			t.Fatalf("late old knowledge remained active: %#v", documents)
		}
	}
}

func TestDigestUnifiesDuplicateKnowledgeAndPreservesBothRecords(t *testing.T) {
	repo := openTestRepository(t)
	fact := "Ragfy uses a persistent Go service for prompt hooks."
	models := &fakeModelEngine{generated: []string{groundedFactJSON(fact, fact), groundedFactJSON(fact, fact)}}
	reconciler := &fakeReconciler{relation: reconciliation.RelationDuplicate}
	service := newTestService(repo, repo, models, 1)
	service.reconciler = reconciler
	directory := t.TempDir()
	digestFactAt(t, service, filepath.Join(directory, "first.jsonl"), "first", "ragfy", "2026-01-02T12:00:00Z", fact)
	digestFactAt(t, service, filepath.Join(directory, "second.jsonl"), "second", "ragfy", "2026-02-02T12:00:00Z", fact)

	documents, err := repo.Documents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	active, superseded := 0, 0
	for _, document := range documents {
		switch document.Status {
		case knowledge.StatusActive:
			active++
		case knowledge.StatusSuperseded:
			superseded++
			if document.ConsolidationReason != "duplicate" || document.SupersededBy == "" {
				t.Fatalf("duplicate provenance missing: %#v", document)
			}
		}
	}
	if len(documents) != 2 || active != 1 || superseded != 1 {
		t.Fatalf("duplicates were not unified: %#v", documents)
	}
}

func TestDigestReconciliationFailurePreservesSessionState(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepository(t)
	first := "Ragfy uses a persistent Go service for prompt hooks."
	second := "Ragfy uses a supervised worker service for prompt hooks."
	models := &fakeModelEngine{generated: []string{groundedFactJSON(first, first), groundedFactJSON(second, second)}}
	reconciler := &fakeReconciler{}
	service := newTestService(repo, repo, models, 1)
	service.reconciler = reconciler
	transcript := filepath.Join(t.TempDir(), "session.jsonl")
	digestFactAt(t, service, transcript, "session", "ragfy", "2026-01-02T12:00:00Z", first)
	firstData, err := os.ReadFile(transcript)
	if err != nil {
		t.Fatal(err)
	}
	reconciler.err = errors.New("reconciliation unavailable")
	writeCodexTranscriptAt(t, transcript, "2026-02-02T12:00:00Z", second, strings.Repeat("Durable implementation context. ", 25))
	if err := service.Digest(ctx, api.HookPayload{SessionID: "session", Source: "codex", Transcript: transcript, CWD: "/workspace/ragfy"}); err == nil {
		t.Fatal("digest succeeded while reconciliation failed")
	}
	documents, err := repo.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 1 || !strings.Contains(documents[0].Content, "persistent Go service") {
		t.Fatalf("failed reconciliation replaced prior knowledge: %#v", documents)
	}
	if done, err := repo.HasSession(ctx, "session", SHA256Hex(firstData)); err != nil || !done {
		t.Fatalf("previous transcript hash was not preserved: done=%v err=%v", done, err)
	}
}

func TestDigestKeepsAmbiguousAndCrossProjectKnowledgeActive(t *testing.T) {
	repo := openTestRepository(t)
	first := "Ragfy stores project configuration in a local repository."
	second := "Another project stores configuration in a remote repository."
	models := &fakeModelEngine{generated: []string{groundedFactJSON(first, first), groundedFactJSON(second, second)}}
	reconciler := &fakeReconciler{relation: reconciliation.RelationSupersedes}
	service := newTestService(repo, repo, models, 1)
	service.reconciler = reconciler
	directory := t.TempDir()
	digestFactAt(t, service, filepath.Join(directory, "ragfy.jsonl"), "ragfy-session", "ragfy", "2026-01-02T12:00:00Z", first)
	digestFactAt(t, service, filepath.Join(directory, "other.jsonl"), "other-session", "other", "2026-02-02T12:00:00Z", second)

	documents, err := repo.Documents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 2 || documents[0].Status != knowledge.StatusActive || documents[1].Status != knowledge.StatusActive || len(reconciler.requests[1].Existing) != 0 {
		t.Fatalf("cross-project knowledge was reconciled: documents=%#v request=%#v", documents, reconciler.requests[1])
	}

	// An empty reconciliation result represents an ambiguous or complementary pair.
	third := "Ragfy stores configuration and operational metadata locally."
	models.generated = append(models.generated, groundedFactJSON(third, third))
	reconciler.relation = ""
	digestFactAt(t, service, filepath.Join(directory, "ambiguous.jsonl"), "ambiguous", "ragfy", "2026-03-02T12:00:00Z", third)
	documents, err = repo.Documents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	active := 0
	for _, document := range documents {
		if document.Status == knowledge.StatusActive {
			active++
		}
	}
	if active != 3 {
		t.Fatalf("ambiguous knowledge was superseded: %#v", documents)
	}
}

func TestTranscriptChronologyUsesMessageTimestamp(t *testing.T) {
	data := []byte("{\"timestamp\":\"2026-01-02T12:00:00Z\"}\n{\"payload\":{\"timestamp\":\"2026-01-03T12:00:00Z\"}}\n")
	got := digest.TranscriptTimestamp(data, time.Unix(1, 0))
	if want := "2026-01-03T12:00:00Z"; got.Format(time.RFC3339) != want {
		t.Fatalf("timestamp=%s want=%s", got, want)
	}
}

func TestDigestFailurePreservesPreviouslyStoredKnowledge(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepository(t)
	evidence := "Ragfy runs as a persistent local Go daemon for prompt hooks."
	models := &fakeModelEngine{generated: []string{groundedFactJSON(evidence, evidence), `{`, `still not JSON`}}
	service := newTestService(repo, repo, models, 1)
	transcript := filepath.Join(t.TempDir(), "session.jsonl")
	writeCodexTranscript(t, transcript, evidence, strings.Repeat("Durable context. ", 40))
	payload := api.HookPayload{SessionID: "session-1", Transcript: transcript, CWD: "/workspace/ragfy"}
	if err := service.Digest(ctx, payload); err != nil {
		t.Fatal(err)
	}

	writeCodexTranscript(t, transcript, evidence, strings.Repeat("Changed context. ", 40))
	if err := service.Digest(ctx, payload); err == nil {
		t.Fatal("digest accepted two invalid model responses")
	}
	docs, err := repo.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || !strings.Contains(docs[0].Content, "persistent local Go daemon") {
		t.Fatalf("previous knowledge was lost after failed digest: %#v", docs)
	}
}

func TestDigestRetriesOnceAfterTruncatedModelJSON(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepository(t)
	evidence := "Ragfy uses a local SQLite repository for durable knowledge."
	models := &fakeModelEngine{generated: []string{`{"problems":[],"facts":[`, groundedFactJSON(evidence, evidence)}}
	service := newTestService(repo, repo, models, 1)
	transcript := filepath.Join(t.TempDir(), "session.jsonl")
	writeCodexTranscript(t, transcript, evidence, strings.Repeat("Technical discussion. ", 35))
	if err := service.Digest(ctx, api.HookPayload{SessionID: "retry", Transcript: transcript, CWD: "/workspace/ragfy"}); err != nil {
		t.Fatal(err)
	}
	if models.generateCalls != 2 {
		t.Fatalf("generate calls = %d, want 2", models.generateCalls)
	}
}

func TestDigestShortTranscriptClearsOldSessionWithoutLoadingModel(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepository(t)
	if err := repo.Upsert(ctx, knowledge.Document{
		Source: "transcripts", SourceID: "old", Title: "old", Content: "old session knowledge",
		Scope: knowledge.ScopeProject, Project: "ragfy", Metadata: map[string]any{"session_id": "short-session"}, Vector: []float32{1, 0},
	}); err != nil {
		t.Fatal(err)
	}
	models := &fakeModelEngine{}
	service := newTestService(repo, repo, models, 1)
	transcript := filepath.Join(t.TempDir(), "short.jsonl")
	writeCodexTranscript(t, transcript, "Short session with no durable knowledge.")
	data, err := os.ReadFile(transcript)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Digest(ctx, api.HookPayload{SessionID: "short-session", Transcript: transcript, CWD: "/workspace/ragfy"}); err != nil {
		t.Fatal(err)
	}
	if models.generateCalls != 0 || models.embedCalls != 0 {
		t.Fatalf("short transcript loaded models: generate=%d embed=%d", models.generateCalls, models.embedCalls)
	}
	docs, err := repo.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Status != knowledge.StatusSuperseded {
		t.Fatalf("old session knowledge was not retired: %#v", docs)
	}
	if done, err := repo.HasSession(ctx, "short-session", SHA256Hex(data)); err != nil || !done {
		t.Fatalf("short session not marked: done=%v err=%v", done, err)
	}
}

func TestHTTPHandlersRejectInvalidPayloads(t *testing.T) {
	repository := openTestRepository(t)
	service := newTestService(repository, repository, &fakeModelEngine{}, 1)
	handler := service.Routes("secret")
	for _, path := range []string{"/digest", "/ingest"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"broken"`))
		req.Header.Set("Authorization", "Bearer secret")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s invalid payload status = %d", path, response.Code)
		}
	}
}

func TestIngestPathReadsSupportedFilesAndStoresChunks(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepository(t)
	evidence := "Ragfy documentation paragraph."
	models := &fakeModelEngine{generated: []string{groundedGlobalFactJSON("Ragfy has durable documentation.", evidence)}}
	service := newTestService(repo, repo, models, defaultQueueCapacity)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte(strings.Repeat("Ragfy documentation paragraph. ", 60)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ignored.go"), []byte("package ignored"), 0600); err != nil {
		t.Fatal(err)
	}
	count, err := service.IngestPath(ctx, api.IngestPayload{Path: root})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("ingested %d records, want 1", count)
	}
	docs, err := repo.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != count {
		t.Fatalf("stored %d documents, count=%d", len(docs), count)
	}
	for _, doc := range docs {
		if doc.Scope != knowledge.ScopeGlobal || doc.Project != "" || doc.Metadata["path"] != filepath.Join(root, "README.md") || doc.Metadata["origin_kind"] != "file" {
			t.Fatalf("unexpected ingested document: %#v", doc)
		}
	}
	sources, err := repo.Sources(ctx)
	if err != nil || len(sources) != 1 || sources[0].Title != "README" || !strings.Contains(sources[0].Content, evidence) {
		t.Fatalf("file source was not preserved: %#v err=%v", sources, err)
	}
}

func TestIngestPathCanRestrictDocumentsToProject(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepository(t)
	evidence := "Project-specific architecture and implementation details."
	service := newTestService(repo, repo, &fakeModelEngine{generated: []string{groundedFactJSON(evidence, evidence)}}, defaultQueueCapacity)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "architecture.md"), []byte("Project-specific architecture and implementation details."), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.IngestPath(ctx, api.IngestPayload{Path: root, Project: " ragfy "}); err != nil {
		t.Fatal(err)
	}
	documents, err := repo.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 1 || documents[0].Scope != knowledge.ScopeProject || documents[0].Project != "ragfy" {
		t.Fatalf("documents=%#v", documents)
	}
}

func TestIngestTextStoresManualProjectMemory(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepository(t)
	service := newTestService(repo, repo, &fakeModelEngine{generated: []string{groundedFactJSON("Production uses PostgreSQL 17.", "Production uses PostgreSQL 17.")}}, defaultQueueCapacity)
	payload := api.IngestPayload{Text: "Production uses PostgreSQL 17.", Title: "Production database", Project: " ragfy "}
	count, err := service.IngestText(ctx, payload)
	if err != nil {
		t.Fatal(err)
	}
	documents, err := repo.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || len(documents) != 1 {
		t.Fatalf("count=%d documents=%#v", count, documents)
	}
	document := documents[0]
	if document.Source != "transcripts" || !strings.Contains(document.Content, payload.Text) || document.Scope != knowledge.ScopeProject || document.Project != "ragfy" || document.Metadata["origin_kind"] != "manual" {
		t.Fatalf("manual document = %#v", document)
	}
	sources, err := repo.Sources(ctx)
	if err != nil || len(sources) != 1 || sources[0].Title != "Production database" || sources[0].Content != payload.Text {
		t.Fatalf("manual source was not preserved: %#v err=%v", sources, err)
	}
}

func TestIngestTextIsIdempotentAndPreservesChangedSourceVersions(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepository(t)
	first := "Production uses PostgreSQL 17."
	second := "Production uses PostgreSQL 18."
	models := &fakeModelEngine{generated: []string{groundedFactJSON(first, first), groundedFactJSON(second, second)}}
	service := newTestService(repo, repo, models, defaultQueueCapacity)

	request := api.IngestPayload{Text: first, Title: "Production database", Project: "ragfy"}
	if count, err := service.IngestText(ctx, request); err != nil || count != 1 {
		t.Fatalf("first ingest count=%d err=%v", count, err)
	}
	if count, err := service.IngestText(ctx, request); err != nil || count != 0 {
		t.Fatalf("unchanged ingest count=%d err=%v", count, err)
	}
	request.Text = second
	if count, err := service.IngestText(ctx, request); err != nil || count != 1 {
		t.Fatalf("changed ingest count=%d err=%v", count, err)
	}
	if models.generateCalls != 2 {
		t.Fatalf("generate calls=%d, want 2", models.generateCalls)
	}

	sources, err := repo.Sources(ctx)
	if err != nil || len(sources) != 2 || sources[0].Content != first || sources[1].Content != second {
		t.Fatalf("preserved sources=%#v err=%v", sources, err)
	}
	documents, err := repo.Documents(ctx)
	if err != nil || len(documents) != 2 || documents[0].Status != knowledge.StatusSuperseded || documents[1].Status != knowledge.StatusActive {
		t.Fatalf("versioned documents=%#v err=%v", documents, err)
	}
	hits, err := retrieval.NewHybrid(repo).Search(ctx, retrieval.Query{Text: "PostgreSQL", Vector: []float32{1, 0}, Limit: 4, Project: "ragfy"})
	if err != nil || len(hits) != 1 || !strings.Contains(hits[0].Doc.Content, "18") {
		t.Fatalf("search hits=%#v err=%v", hits, err)
	}
}

func TestRefreshEmbeddingsRebuildsOnlyForChangedModel(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepository(t)
	if err := repo.Upsert(ctx, knowledge.Document{Source: "documents", SourceID: "one", Title: "One", Content: "content", Scope: knowledge.ScopeGlobal, Vector: []float32{0, 1}}); err != nil {
		t.Fatal(err)
	}
	models := &fakeModelEngine{}
	if err := RefreshEmbeddings(ctx, repo, models, testEmbeddingVersion); err != nil {
		t.Fatal(err)
	}
	if models.embedCalls != 2 {
		t.Fatalf("first refresh embed calls = %d, want validation plus rebuild", models.embedCalls)
	}
	if err := RefreshEmbeddings(ctx, repo, models, testEmbeddingVersion); err != nil {
		t.Fatal(err)
	}
	if models.embedCalls != 3 {
		t.Fatalf("second refresh should only validate; calls=%d", models.embedCalls)
	}
}

func TestRecallReturnsEmptyContextWhenEmbeddingFails(t *testing.T) {
	repository := openTestRepository(t)
	service := newTestService(repository, repository, &fakeModelEngine{embedErr: errors.New("embedding unavailable")}, 1)
	response := requestJSON(t, service.Routes("secret"), http.MethodPost, "/recall", "secret", api.HookPayload{Prompt: "A sufficiently long recall query"})
	if response.Code != http.StatusOK || response.Body.String() != "{}\n" {
		t.Fatalf("embedding failure response = %d %q", response.Code, response.Body.String())
	}
}
