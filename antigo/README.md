# Personal knowledge base for Claude Code and Codex

This is a small retrieval layer for your own material. It reads the sources you select, stores short chunks, and adds relevant chunks to Claude Code and Codex prompts. When your library does not contain an answer, the hook returns nothing.

Open this folder in Claude Code or Codex and run:

```bash
python setup.py
```

The walkthrough asks what you want to recall, whether to use free local or paid embeddings, which sources to include, whether to import old material, and when to refresh it. It detects Windows or macOS for you.

SQLite stays on your computer. Free local embeddings are the default. The local embedding model downloads once, then your source text does not go to an embedding API. Paid embeddings remain available if you choose them.

Before any full ingestion, the walkthrough counts your material and shows the estimated tokens, time, storage, and paid cost. Paid embedding needs a typed `yes` after you see the estimate.

## Sources

Choose any combination of these sources during setup:

- A folder of text, Markdown, or PDFs
- An Obsidian vault
- Individual YouTube video URLs with available transcripts
- Claude Code session history
- Codex session history

If you need another source, write a connector that returns a title, text, timestamp, source name, and metadata. The retrieval engine does not need to change.

## After setup

Start a new Claude Code or Codex session, then ask questions that you know appear in your selected sources. The hook logs the exact chunks it adds to each prompt. After you have a few real prompts, review them:

```bash
python chunk_review.py --hours 24
```

Mark each injected chunk Useful or Junk and click Save. Once you have reviewed at least three prompts and eight chunks, the installer replays those prompts at three, four, and six chunks. It writes the smallest limit that keeps useful chunks in `config.json`. Review it again after you add a substantially different source.

If you choose a daily or weekly refresh, the walkthrough creates a user schedule on macOS or Windows. Your computer must be awake when the scheduled time arrives. The refresh log is in `logs/refresh.log`.
