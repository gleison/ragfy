<p align="center">
  <img src="assets/icons/ragfy.png" alt="Ragfy brain with internal connections" width="300">
</p>

<h1 align="center">Ragfy</h1>

<p align="center"><strong>Your local second brain for Claude Code and Codex.</strong></p>

A local knowledge base for Claude Code and Codex. A Go service keeps the embedding model loaded; hooks make short calls to the service to retrieve context before a prompt and digest the session when it ends. Session extraction runs through the same agent provider that created the transcript.

## Install

The installer detects macOS or Linux and the current CPU architecture, downloads the matching release, verifies its SHA-256 checksum, and installs Ragfy atomically in `/usr/local/bin`:

```sh
curl -fsSL https://github.com/gleison/ragfy/releases/latest/download/install.sh | sh
```

The final step starts the interactive setup wizard. Use `--user` to install in `~/.local/bin` without elevated permissions, or `--prefix` to choose another location:

```sh
curl -fsSL https://github.com/gleison/ragfy/releases/latest/download/install.sh | sh -s -- --user
```

After setup, running `ragfy` with no arguments shows the current installation status. Operational commands detect a missing setup and point to `ragfy setup` instead of partially configuring the application.

## Build from source

Requirements: Go 1.25+, Git, CMake, a C/C++ compiler, `pkg-config`, and at least one authenticated Codex or Claude Code CLI. The local embedding runtime uses `llama.cpp` through the `go-llama` Go binding.

```sh
./scripts/build-ragfy.sh
./scripts/install-ragfy.sh
```

The build script compiles the native runtime for the current operating system and architecture and embeds the Git version, commit, and build date. The developer install script replaces the executable atomically and reloads an existing user service; this matters on macOS, where overwriting a binary used by a LaunchAgent can make the kernel reject it with `OS_REASON_CODESIGNING`. Set `RAGFY_BIN_DIR` to choose a development install location. On Windows, the native build still requires manual CMake, compiler, and pkg-config setup.

## Setup and use

```sh
ragfy setup
ragfy status
ragfy status --verbose
ragfy status --json
ragfy update
ragfy search "How is authentication configured?"
ragfy ingest ~/Documents/Knowledge
ragfy ingest --project ragfy ./docs
ragfy ingest --text "Production uses PostgreSQL 17."
ragfy ingest --provider claude --text "Production uses PostgreSQL 17."
```

`setup` is an interactive wizard. It suggests the default embedding model, lets you enter another GGUF model URI, and lets you select Codex, Claude Code, or both. It validates the selected CLI and embedding model, saves the configuration, reconciles the selected hooks, registers a user-level service (LaunchAgent on macOS, systemd on Linux, or a logon task on Windows), and waits until the service reports healthy. The service listens only on `127.0.0.1` and protects its API with a local token.

`status` combines local installation checks with live service information. It reports the binary and data paths, service registration and health, model cache, selected integrations and hooks, queue depth, active and superseded knowledge, preserved sources, digested sessions, and project count. `--json` provides the same report for automation.

`update` downloads the current installer over HTTPS and asks it to replace a direct installation in the same prefix. Homebrew-managed installations are left to `brew upgrade ragfy`.

`search` runs the same hybrid semantic and lexical search used by the prompt hook and prints the matching active memories. Global memories are always searched; pass `--project <name>` to also search memories scoped to a project.

The integration selection is the desired state. Running `setup` again adds hooks for selected clients and removes Ragfy hooks from deselected clients while preserving unrelated settings. `setup-hooks` applies the saved selection without rerunning the wizard.

`ingest` processes `.md`, `.rst`, and `.txt` files as global knowledge by default. It can also process a phrase or note directly with `--text`, optionally using `--title`. Pass `--project <name>` to restrict extracted knowledge to that project. Files, manual text, and session digests all use the same extraction, evidence validation, reconciliation, embedding, and persistence pipeline. When both integrations are configured, ingest prefers Codex; `--provider codex|claude` selects one explicitly. `ragfy uninstall` removes the service startup registration and Ragfy hooks from both clients. It preserves the database, downloaded models, logs, and unrelated hook configuration.

## Models and local data

By default, the setup wizard downloads the following embedding GGUF model to the application data directory and verifies its SHA-256 hash:

- Qwen3-Embedding-0.6B Q8_0 for multilingual embeddings (about 639 MB).

This model is the setup wizard's recommended preset. A custom `hf://` or `https://` GGUF URI can be entered instead. Ragfy verifies that the file is GGUF, loads it in embedding mode, generates test vectors, and checks their dimensions and numeric values before saving the choice. The known Qwen preset uses its published SHA-256, `last` pooling, and its retrieval query instruction. For a custom model, Ragfy uses the pooling declared by the model, applies no query instruction, and pins the downloaded file's SHA-256 for subsequent starts.

Qwen3-Embedding supports more than 100 languages. Ragfy uses an English retrieval instruction for queries and requires `last` pooling, as specified by the official GGUF. Its weights stay in the local cache while the service is running. Source content, extracted knowledge, embeddings, and recall stay local. Codex sources are processed with `codex exec`; Claude sources are processed with `claude -p`. Both run without tools, hooks, workspace writes, or agent-session persistence. Extraction uses the complete normalized source by default. Oversized sources are windowed with overlap, then candidates are consolidated and validated against the complete content. After extraction, the same provider reconciles semantically close records from earlier sources. Confirmed newer states supersede older ones, while ambiguous or complementary records remain active. Superseded records are retained for audit but excluded from recall.

Ragfy preserves each normalized source version in the same SQLite database: the clean conversational spine for a session, the text of a supported file, or text supplied manually. Extracted memories reference their source version. Reprocessing unchanged content is idempotent; changed content creates a new source version and retires the previous derived memories without deleting either the previous source or its extracted records. Raw sources are never included directly in search results.

SQLite is created as `memory.sqlite` in Ragfy's application data directory:

- macOS: `~/Library/Application Support/Ragfy/memory.sqlite`
- Linux: `$XDG_DATA_HOME/ragfy/memory.sqlite`, or `~/.local/share/ragfy/memory.sqlite` if `XDG_DATA_HOME` is not set.
- Windows: `%LOCALAPPDATA%\Ragfy\memory.sqlite`

Set `RAGFY_HOME` to choose a different directory; the database will be created at `<RAGFY_HOME>/memory.sqlite`. The saved setup choices are stored in `<RAGFY_HOME>/config.json`; the token and the `models/` and `logs/` directories are stored alongside it. Ragfy does not migrate the legacy `kb.sqlite`. When it detects embeddings from an earlier version in the Ragfy database, it rebuilds them from the stored documents before accepting queries. Persistence depends on the `knowledge.Repository` interface. SQLite is selected only in `cmd/ragfy/main.go`, so another adapter such as Supabase can replace it without changing the application service.

## Architecture and legacy files

```text
UserPromptSubmit -> provider hook -> local service -> local embeddings + hybrid search -> context
SessionEnd       -> provider hook -> digestion queue -> preserve source -> extraction -> embedding -> reconciliation -> SQLite
ingest           -> file or text -> preserve source -> extraction -> embedding -> reconciliation -> SQLite
```

The Go source follows the standard command-plus-internal-packages layout:

```text
cmd/ragfy                 executable composition
internal/api              hook transport contract
internal/extraction       replaceable knowledge extraction
internal/reconciliation   replaceable cross-source knowledge reconciliation
internal/knowledge        persistence contract and domain types
internal/storage/sqlite   SQLite adapter and schema
internal/model            generation and embedding contracts, plus llama.cpp
internal/digest           transcript parsing and extraction validation
internal/retrieval        similarity, lexical matching and ranking
internal/service          application and HTTP service
internal/hook             Codex and Claude hook integration
internal/platform         operating-system integration
internal/settings         process-level constants
```

Generation, embedding, extraction, reconciliation and retrieval are injected independently. The default composition routes sources to generators backed by the configured Codex or Claude CLI, while Qwen provides local embeddings. Alternative generators, embedders, extractors, reconcilers, or database-native retrievers can replace one capability without implementing the others. Extracted records are classified as global or project-scoped. Reconciliation compares only compatible records in the same scope and project, uses source chronology rather than processing order, and commits source preservation, derived records, and supersession links atomically. Recall combines global knowledge with knowledge belonging to the current working-directory project, excludes other projects, and ignores superseded records and preserved raw sources.

The default retriever combines semantic similarity with field-aware BM25, gives titles more lexical weight, applies a small current-project boost, drops candidates that fall materially below the best result, and uses MMR to avoid redundant context. It may return fewer than four records when the remaining candidates are weak.

The previous Python project is preserved in [`antigo/`](antigo/). Its connectors and evaluation and ingestion scripts are not used by the Go hooks.
