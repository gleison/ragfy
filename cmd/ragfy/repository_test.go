package main

import (
	"context"
	"path/filepath"
	"testing"
)

func openTestRepository(t *testing.T) *SQLiteRepository {
	t.Helper()
	repo, err := OpenSQLite(filepath.Join(t.TempDir(), "memory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

func TestSQLiteRepositoryRoundTripSearchAndModelVersion(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepository(t)
	docs := []Document{
		{Source: "transcripts", SourceID: "service", Title: "Persistent service", Content: "The hook calls a resident Go daemon.", Kind: "decision", Area: "work", Metadata: map[string]any{"session_id": "s1"}, Vector: []float32{1, 0}},
		{Source: "documents", SourceID: "database", Title: "SQLite storage", Content: "Knowledge is stored locally.", Area: "documents", Metadata: map[string]any{"path": "/docs/storage.md"}, Vector: []float32{0, 1}},
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
	if len(stored) != 2 || stored[0].Metadata["session_id"] != "s1" {
		t.Fatalf("unexpected stored documents: %#v", stored)
	}

	hits, err := repo.Search(ctx, "resident daemon hook", []float32{1, 0}, 4)
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
	hits, err = repo.Search(ctx, "SQLite storage", []float32{1, 0}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Doc.SourceID != "database" {
		t.Fatalf("updated embedding was not searchable: %#v", hits)
	}
}

func TestSQLiteRepositorySessionDeduplicationAndScopedDeletion(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepository(t)
	for _, doc := range []Document{
		{Source: "transcripts", SourceID: "s1-doc", Title: "one", Content: "session one knowledge", Metadata: map[string]any{"session_id": "s1"}, Vector: []float32{1}},
		{Source: "transcripts", SourceID: "s10-doc", Title: "ten", Content: "session ten knowledge", Metadata: map[string]any{"session_id": "s10"}, Vector: []float32{1}},
		{Source: "documents", SourceID: "manual", Title: "manual", Content: "unrelated document", Metadata: map[string]any{"session_id": "s1"}, Vector: []float32{1}},
	} {
		if err := repo.Upsert(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}

	if done, err := repo.HasSession(ctx, "s1", "hash-a"); err != nil || done {
		t.Fatalf("unmarked session done=%v err=%v", done, err)
	}
	if _, err := repo.MarkSession(ctx, "s1", "hash-a"); err != nil {
		t.Fatal(err)
	}
	if done, err := repo.HasSession(ctx, "s1", "hash-a"); err != nil || !done {
		t.Fatalf("marked session done=%v err=%v", done, err)
	}
	if done, err := repo.HasSession(ctx, "s1", "hash-b"); err != nil || done {
		t.Fatalf("changed transcript done=%v err=%v", done, err)
	}

	if err := repo.DeleteSessionDocs(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	docs, err := repo.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 {
		t.Fatalf("scoped deletion left %d docs, want 2: %#v", len(docs), docs)
	}
	for _, doc := range docs {
		if doc.SourceID == "s1-doc" {
			t.Fatalf("session s1 transcript survived deletion: %#v", docs)
		}
	}
}

func TestSQLiteRepositoryUpsertReplacesExistingDocument(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepository(t)
	doc := Document{Source: "documents", SourceID: "same", Title: "old", Content: "old content", Vector: []float32{1}}
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
