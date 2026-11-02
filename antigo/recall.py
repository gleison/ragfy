#!/usr/bin/env python3
"""UserPromptSubmit hook — injects top KB hits above every prompt.

Replaces the Brain's prompt_router. Reads the hook payload from stdin,
hybrid-searches the KB (BM25 + embeddings + RRF + age decay), prints a
compact recall block (stdout becomes injected context). Must never block
a prompt: any failure degrades to lexical-only or to silence, exit 0.
"""
import json
import os
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))

SNIPPET = 220


def _limit():
    import config

    return int(config.load().get("retrieval", {}).get("limit", 6))


def lexical_only(db, store, query, limit):
    """Offline / embedding-failure fallback. Without vectors there is no
    similarity floor to gate on, so this stays deliberately narrow: content
    terms only, and nothing at all when the prompt has none."""
    match = store._fts_query(query)
    if not match:
        return []
    try:
        rows = db.execute(
            """SELECT docs.* FROM docs_fts JOIN docs ON docs.id = docs_fts.rowid
               WHERE docs_fts MATCH ? ORDER BY docs_fts.rank LIMIT ?""",
            (match, limit),
        ).fetchall()
        return [(0.0, r) for r in rows]
    except Exception:
        return []


def main():
    if os.environ.get("CLAUDE_KB_DISTILL"):
        return  # never inject into KB distillation subprocesses
    try:
        payload = json.load(sys.stdin)
    except ValueError:
        return
    prompt = (payload.get("prompt") or "").strip()
    if len(prompt) < 12:
        return
    import config  # noqa: F401 — loads .env (OpenAI key) into the environment
    import store

    limit = _limit()
    db = store.connect()
    try:
        results = store.search(db, prompt, limit=limit, log=True,
                               session=payload.get("session_id"), agent="main")
    except Exception:
        results = lexical_only(db, store, prompt, limit)  # offline / embed failure
    # Returning nothing is a valid, common outcome now: the retrieval floor
    # means an off-topic prompt injects no context at all instead of eight
    # confident non-answers.
    if not results:
        return
    print(f"=== KB RECALL ({len(results)} match{'' if len(results)==1 else 'es'} from Mason's knowledge base, ranked by relevance) ===")
    for _, row in results:
        snippet = " ".join((row["content"] or "").split())[:SNIPPET]
        kind = row["kind"] or row["source"]
        print(f"[{kind} {(row['ts'] or '')[:10]}] {row['title']} — {snippet}")


if __name__ == "__main__":
    try:
        main()
    except Exception:
        pass
    sys.exit(0)
