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

	"ragfy/internal/api"
	"ragfy/internal/chunking"
	"ragfy/internal/extraction"
	"ragfy/internal/knowledge"
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
		Chunker:    chunking.NewText(1200, 160),
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
	if health.Code != http.StatusOK || !strings.Contains(health.Body.String(), `"ok":true`) {
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
	t.Helper()
	var lines []string
	for _, message := range messages {
		row := map[string]any{"type": "event_msg", "payload": map[string]any{"type": "user_message", "message": message}}
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

func groundedFactJSON(fact, evidence string) string {
	return fmt.Sprintf(`{"problems":[],"facts":[{"fact":%q,"area":"runtime","status":"current","evidence":%q,"visibility":"project"}],"preferences":[],"decisions":[]}`, fact, evidence)
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
	if len(docs) != 1 || !strings.Contains(docs[0].Content, "SQLite repository") {
		t.Fatalf("session knowledge was not replaced: %#v", docs)
	}
	if docs[0].Scope != knowledge.ScopeProject || docs[0].Project != "ragfy" || docs[0].Metadata["session_id"] != "session-1" || docs[0].Metadata["origin_project"] != "ragfy" || docs[0].Metadata["agent_source"] != "codex" || docs[0].Metadata["evidence"] != secondEvidence {
		t.Fatalf("missing digest metadata: %#v", docs[0].Metadata)
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
	if len(docs) != 0 {
		t.Fatalf("old session knowledge survived: %#v", docs)
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
	models := &fakeModelEngine{}
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
	if count < 2 {
		t.Fatalf("ingested %d chunks, want at least 2", count)
	}
	docs, err := repo.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != count {
		t.Fatalf("stored %d documents, count=%d", len(docs), count)
	}
	for _, doc := range docs {
		if doc.Title != "README" || doc.Scope != knowledge.ScopeGlobal || doc.Project != "" || doc.Metadata["path"] != filepath.Join(root, "README.md") {
			t.Fatalf("unexpected ingested document: %#v", doc)
		}
	}
}

func TestIngestPathCanRestrictDocumentsToProject(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepository(t)
	service := newTestService(repo, repo, &fakeModelEngine{}, defaultQueueCapacity)
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
