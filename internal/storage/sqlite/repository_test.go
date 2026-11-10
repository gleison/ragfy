package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"ragfy/internal/knowledge"
	"ragfy/internal/retrieval"
)

func testSource(id, kind, key string) knowledge.Source {
	return knowledge.Source{ID: id, Kind: kind, Key: key, Content: "source content " + id, ContentHash: "hash-" + id, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
}

func TestOpenMigratesExistingRagfyDatabaseForSupersession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.sqlite")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.Exec(`CREATE TABLE docs (
id INTEGER PRIMARY KEY, source TEXT NOT NULL, source_id TEXT NOT NULL,
ts TEXT, title TEXT, content TEXT NOT NULL, metadata TEXT, kind TEXT, area TEXT,
scope TEXT NOT NULL, project TEXT NOT NULL, embedding BLOB, UNIQUE(source,source_id));`)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	repo, err := Open(Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Upsert(context.Background(), knowledge.Document{Source: "transcripts", SourceID: "migrated", Content: "existing knowledge", Scope: knowledge.ScopeGlobal, Vector: []float32{1}}); err != nil {
		t.Fatal(err)
	}
	documents, err := repo.Documents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 1 || documents[0].Status != knowledge.StatusActive {
		t.Fatalf("migration did not initialize active status: %#v", documents)
	}
}

func openTestRepository(t *testing.T) *Repository {
	t.Helper()
	repo, err := Open(Config{Path: filepath.Join(t.TempDir(), "memory.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

func TestRepositoryStatistics(t *testing.T) {
	repository := openTestRepository(t)
	ctx := context.Background()
	documents := []knowledge.Document{
		{Source: "manual", SourceID: "active", Content: "active", Scope: knowledge.ScopeGlobal, Status: knowledge.StatusActive},
		{Source: "manual", SourceID: "old", Content: "old", Scope: knowledge.ScopeProject, Project: "ragfy", Status: knowledge.StatusSuperseded},
	}
	for _, document := range documents {
		if err := repository.Upsert(ctx, document); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.CommitSession(ctx, knowledge.SessionCommit{SessionID: "session", TranscriptHash: "hash", Source: testSource("session-source", "session", "session")}); err != nil {
		t.Fatal(err)
	}
	statistics, err := repository.Statistics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if statistics.ActiveRecords != 1 || statistics.SupersededRecords != 1 || statistics.DigestedSessions != 1 || statistics.PreservedSources != 1 || statistics.Projects != 1 {
		t.Fatalf("statistics = %#v", statistics)
	}
}

func TestSQLiteRepositoryRoundTripSearchAndModelVersion(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepository(t)
	docs := []knowledge.Document{
		{Source: "transcripts", SourceID: "service", Title: "Persistent service", Content: "The hook calls a resident Go daemon.", Kind: "decision", Area: "work", Scope: knowledge.ScopeProject, Project: "ragfy", Metadata: map[string]any{"session_id": "s1"}, Vector: []float32{1, 0}},
		{Source: "documents", SourceID: "database", Title: "SQLite storage", Content: "Knowledge is stored locally.", Area: "documents", Scope: knowledge.ScopeGlobal, Metadata: map[string]any{"path": "/docs/storage.md"}, Vector: []float32{0, 1}},
	}
	for _, doc := range docs {
		if err := repo.Upsert(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}

	stored, err := repo.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 || stored[0].Metadata["session_id"] != "s1" || stored[0].Scope != knowledge.ScopeProject || stored[0].Project != "ragfy" || stored[1].Scope != knowledge.ScopeGlobal {
		t.Fatalf("unexpected stored documents: %#v", stored)
	}

	retriever := retrieval.NewHybrid(repo)
	hits, err := retriever.Search(ctx, retrieval.Query{Text: "resident daemon hook", Vector: []float32{1, 0}, Limit: 4, Project: "ragfy"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Doc.SourceID != "service" {
		t.Fatalf("unexpected search results: %#v", hits)
	}

	if model, err := repo.EmbeddingModel(ctx); err != nil || model != "" {
		t.Fatalf("initial model = %q, err=%v", model, err)
	}
	if err := repo.SetEmbeddingModel(ctx, "model-v1"); err != nil {
		t.Fatal(err)
	}
	if model, err := repo.EmbeddingModel(ctx); err != nil || model != "model-v1" {
		t.Fatalf("stored model = %q, err=%v", model, err)
	}

	if err := repo.UpdateEmbedding(ctx, "documents", "database", []float32{1, 0}); err != nil {
		t.Fatal(err)
	}
	hits, err = retriever.Search(ctx, retrieval.Query{Text: "SQLite storage", Vector: []float32{1, 0}, Limit: 1, Project: "other"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Doc.SourceID != "database" {
		t.Fatalf("updated embedding was not searchable: %#v", hits)
	}
}

func TestSQLiteRepositoryRejectsInvalidScope(t *testing.T) {
	repo := openTestRepository(t)
	for _, document := range []knowledge.Document{
		{Source: "documents", SourceID: "missing", Content: "missing scope"},
		{Source: "documents", SourceID: "global-project", Content: "global with project", Scope: knowledge.ScopeGlobal, Project: "ragfy"},
		{Source: "documents", SourceID: "project-missing", Content: "project without name", Scope: knowledge.ScopeProject},
	} {
		if err := repo.Upsert(context.Background(), document); err == nil {
			t.Fatalf("accepted invalid document: %#v", document)
		}
	}
}

func TestSQLiteRepositorySessionDeduplicationAndScopedRetirement(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepository(t)
	for _, doc := range []knowledge.Document{
		{Source: "transcripts", SourceID: "s1-doc", Title: "one", Content: "session one knowledge", Scope: knowledge.ScopeProject, Project: "ragfy", Metadata: map[string]any{"session_id": "s1"}, Vector: []float32{1}},
		{Source: "transcripts", SourceID: "s10-doc", Title: "ten", Content: "session ten knowledge", Scope: knowledge.ScopeProject, Project: "ragfy", Metadata: map[string]any{"session_id": "s10"}, Vector: []float32{1}},
		{Source: "documents", SourceID: "manual", Title: "manual", Content: "unrelated document", Scope: knowledge.ScopeGlobal, Metadata: map[string]any{"session_id": "s1"}, Vector: []float32{1}},
	} {
		if err := repo.Upsert(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}

	if done, err := repo.HasSession(ctx, "s1", "hash-a"); err != nil || done {
		t.Fatalf("unmarked session done=%v err=%v", done, err)
	}
	if err := repo.CommitSession(ctx, knowledge.SessionCommit{SessionID: "s1", TranscriptHash: "hash-a", Source: testSource("s1-source", "session", "s1")}); err != nil {
		t.Fatal(err)
	}
	if done, err := repo.HasSession(ctx, "s1", "hash-a"); err != nil || !done {
		t.Fatalf("marked session done=%v err=%v", done, err)
	}
	if done, err := repo.HasSession(ctx, "s1", "hash-b"); err != nil || done {
		t.Fatalf("changed transcript done=%v err=%v", done, err)
	}

	docs, err := repo.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 3 {
		t.Fatalf("scoped retirement left %d docs, want 3: %#v", len(docs), docs)
	}
	for _, doc := range docs {
		if doc.SourceID == "s1-doc" && doc.Status != knowledge.StatusSuperseded {
			t.Fatalf("session s1 transcript remained active: %#v", docs)
		}
		if doc.SourceID != "s1-doc" && doc.Status == knowledge.StatusSuperseded {
			t.Fatalf("unrelated document was retired: %#v", docs)
		}
	}
}

func TestSQLiteRepositoryUpsertReplacesExistingDocument(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepository(t)
	doc := knowledge.Document{Source: "documents", SourceID: "same", Title: "old", Content: "old content", Scope: knowledge.ScopeGlobal, Vector: []float32{1}}
	if err := repo.Upsert(ctx, doc); err != nil {
		t.Fatal(err)
	}
	doc.Title, doc.Content, doc.Vector = "new", "new content", []float32{0.5}
	if err := repo.Upsert(ctx, doc); err != nil {
		t.Fatal(err)
	}
	docs, err := repo.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Title != "new" || docs[0].Content != "new content" {
		t.Fatalf("upsert did not replace document: %#v", docs)
	}
}

func TestSQLiteCommitSessionIsAtomicAndRestoresReplacedWinnerDependents(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepository(t)
	documents := []knowledge.Document{
		{Source: "transcripts", SourceID: "winner", Title: "winner", Content: "winner", Scope: knowledge.ScopeProject, Project: "ragfy", Status: knowledge.StatusActive, Metadata: map[string]any{"session_id": "replace"}, Vector: []float32{1}},
		{Source: "transcripts", SourceID: "dependent", Title: "dependent", Content: "dependent", Scope: knowledge.ScopeProject, Project: "ragfy", Status: knowledge.StatusSuperseded, SupersededBy: "winner", Metadata: map[string]any{"session_id": "other"}, Vector: []float32{1}},
	}
	for _, document := range documents {
		if err := repo.Upsert(ctx, document); err != nil {
			t.Fatal(err)
		}
	}
	invalid := knowledge.Document{Source: "transcripts", SourceID: "new", Title: "new", Content: "new", Scope: knowledge.ScopeProject, Project: "ragfy", Metadata: map[string]any{"session_id": "replace"}, Vector: []float32{1}}
	err := repo.CommitSession(ctx, knowledge.SessionCommit{
		SessionID: "replace", TranscriptHash: "hash-b", Source: testSource("replace-b", "session", "replace"), Documents: []knowledge.Document{invalid},
		Supersessions: []knowledge.Supersession{{LoserSourceID: "missing", WinnerSourceID: "new", Reason: "supersedes"}},
	})
	if err == nil {
		t.Fatal("commit accepted a missing supersession loser")
	}
	stored, err := repo.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 || stored[0].SourceID != "winner" || stored[1].Status != knowledge.StatusSuperseded {
		t.Fatalf("failed transaction changed documents: %#v", stored)
	}

	if err := repo.CommitSession(ctx, knowledge.SessionCommit{SessionID: "replace", TranscriptHash: "hash-c", Source: testSource("replace-c", "session", "replace"), Documents: []knowledge.Document{invalid}}); err != nil {
		t.Fatal(err)
	}
	stored, err = repo.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 3 || stored[0].SourceID != "winner" || stored[0].Status != knowledge.StatusSuperseded || stored[1].SourceID != "dependent" || stored[1].Status != knowledge.StatusActive || stored[2].SourceID != "new" {
		t.Fatalf("replacement did not restore dependent: %#v", stored)
	}
}
