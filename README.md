# Ragfy

A local knowledge base for Claude Code and Codex. A Go service keeps the embedding and digestion models loaded; hooks make short calls to the service to retrieve context before a prompt and digest the session when it ends.

## Build

Requirements: Go 1.25+, Git, CMake, a C/C++ compiler, and `pkg-config`. The inference runtime uses `llama.cpp` through the `go-llama` Go binding.

```sh
./scripts/build-ragfy.sh
mkdir -p "$HOME/.local/bin"
cp dist/ragfy "$HOME/.local/bin/ragfy"
```

The script builds the native runtime for the current operating system and architecture. Add `~/.local/bin` to your `PATH`. On Windows, the native build still requires manual CMake, compiler, and pkg-config setup.

## Install and use

```sh
ragfy setup
ragfy status
ragfy ingest ~/Documents/Knowledge
```

`setup` registers a user-level service (LaunchAgent on macOS, systemd on Linux, or a logon task on Windows) and configures the `UserPromptSubmit` and `SessionEnd` hooks for Claude Code and Codex. The service listens only on `127.0.0.1` and protects its API with a local token.

`ingest` indexes `.md`, `.rst`, and `.txt` files. `ragfy uninstall` removes the service startup registration. To remove the hooks, edit the `hooks` section in `~/.claude/settings.json` and `~/.codex/hooks.json`.

## Models and local data

On first startup, Ragfy downloads the fixed MVP GGUF models to the application data directory and verifies their SHA-256 hashes:

- Qwen3-Embedding-0.6B Q8_0 for multilingual embeddings (about 639 MB).
- Qwen3-4B Q4_K_M for transcript digestion (about 2.5 GB).

Qwen3-Embedding supports more than 100 languages. Ragfy uses an English retrieval instruction for queries and requires `last` pooling, as specified by the official GGUF. The weights stay in the local cache, and both models remain loaded while the service is running. Prompts and transcripts are not sent to a model API.

SQLite is created as `memory.sqlite` in Ragfy's application data directory:

- macOS: `~/Library/Application Support/Ragfy/memory.sqlite`
- Linux: `$XDG_DATA_HOME/ragfy/memory.sqlite`, or `~/.local/share/ragfy/memory.sqlite` if `XDG_DATA_HOME` is not set.
- Windows: `%LOCALAPPDATA%\Ragfy\memory.sqlite`

Set `RAGFY_HOME` to choose a different directory; the database will be created at `<RAGFY_HOME>/memory.sqlite`. The token and the `models/` and `logs/` directories are stored alongside it. Ragfy does not migrate the legacy `kb.sqlite`. When it detects embeddings from an earlier version in the Ragfy database, it rebuilds them from the stored documents before accepting queries. Persistence depends on the `knowledge.Repository` interface. SQLite is selected only in `cmd/ragfy/main.go`, so another adapter such as Supabase can replace it without changing the application service.

## Architecture and legacy files

```text
UserPromptSubmit -> Go hook -> local service -> embeddings + hybrid search -> context
SessionEnd       -> Go hook -> digestion queue -> local Qwen -> typed records -> SQLite
```

The Go source follows the standard command-plus-internal-packages layout:

```text
cmd/ragfy                 executable composition
internal/api              hook transport contract
internal/knowledge        persistence contract and domain types
internal/storage/sqlite   SQLite adapter and schema
internal/model            inference contract and llama.cpp adapter
internal/digest           transcript parsing and knowledge extraction
internal/service          application and HTTP service
internal/hook             Codex and Claude hook integration
internal/platform         operating-system integration
internal/settings         process-level constants
```

The previous Python project is preserved in [`antigo/`](antigo/). Its connectors and evaluation and ingestion scripts are not used by the Go hooks.
