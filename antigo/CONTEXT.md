# How the knowledge base works

The knowledge base reads selected material, splits it into short chunks, embeds each chunk, and stores the result. When you send a Claude Code or Codex prompt, `recall.py` searches the stored chunks and prints only the chunks that clear the retrieval thresholds.

```
selected sources -> chunks -> embeddings -> search index
your prompt -> recall hook -> relevant chunks -> Claude Code or Codex
```

SQLite stores the source chunks and vectors in one local file. The hook reads that file when it needs to attach text to a prompt.

The search combines vector similarity with SQLite full-text matching. It applies the existing similarity floors after ranking. A search result must clear the floor, and the best result must clear a higher floor before the hook adds anything to your prompt. An empty result is correct when your sources do not answer the question.

Long documents are split before embedding. A single vector for a long document averages unrelated sections together. Smaller chunks let retrieval return the passage that contains the answer instead of an entire document.

The review checks chunks that were actually injected into Claude Code or Codex. Run `python chunk_review.py --hours 24`, mark each chunk Useful or Junk, and save the review. After three prompts and eight chunks have labels, the system replays those prompts at three, four, and six chunks and writes the smallest relevant limit to `config.json`. This uses your judgment instead of having an AI grade its own answers.

Refreshes run `ingest.py`. Ingestion is idempotent: unchanged chunks keep their existing embeddings, and changed chunks get embedded again. Scheduled refreshes are local jobs, so they run only while your computer is awake.
