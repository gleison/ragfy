# Ragfy architecture

The Go daemon assembled in `cmd/ragfy` is the active runtime. Application code lives under `internal`: `service` owns the use cases and HTTP API, `knowledge` defines the persistence contract, `storage/sqlite` implements that contract, `model` owns inference, `digest` extracts knowledge, `hook` integrates Codex and Claude, and `platform` handles operating-system concerns. Hook processes are intentionally short-lived and send JSON to a token-protected loopback API.

Digest generation tokenizes Qwen ChatML control tokens with `ParseSpecial=true` and uses an empty thinking prefix. Do not use the binding's native completion path for formatted chat prompts: it tokenizes control tokens as ordinary text. Extraction accepts only complete JSON with all four arrays, rejects schema placeholders and transient runtime observations, and requires every record to cite an exact transcript quote. Preference evidence must come from a user message. The MVP gives the model a balanced 6,500-byte excerpt containing the beginning and end of long conversations; the middle is omitted. Inspect extracted content when validating changes, not just document counts or worker completion logs.

MVP model selections are fixed in the Go app:

- `Qwen/Qwen3-4B-GGUF/Qwen3-4B-Q4_K_M.gguf` for typed transcript extraction.
- `Qwen/Qwen3-Embedding-0.6B-GGUF/Qwen3-Embedding-0.6B-Q8_0.gguf` for multilingual embeddings, using `last` pooling and an English retrieval instruction on queries.

The app downloads missing GGUF files into its application data directory and checks their pinned SHA-256 values before loading. It tracks the embedding model version in the repository and re-embeds stored documents when that version changes. `knowledge.Repository` is the persistence boundary; `storage/sqlite.Repository` is the first implementation and is selected only by `cmd/ragfy`. A Supabase adapter can implement the same contract without changing the service package.

`ragfy setup` registers the background service and both `UserPromptSubmit` and `SessionEnd` hooks for Claude Code and Codex. `ragfy ingest <folder>` indexes Markdown, RST, and text files.
