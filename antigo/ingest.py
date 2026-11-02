"""Read the configured sources, chunk them, store them, embed what is new.

Safe to re-run. Documents are matched on a stable id, so an unchanged file is
skipped, a changed one is re-embedded, and an interrupted run loses only the
batch it was in.

  python ingest.py                 # every source in config.json
  python ingest.py documents       # just one
"""
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
import chunking
import store
from connectors import codex_sessions, documents, obsidian, transcripts, youtube

CONNECTORS = {
    "documents": documents.fetch,
    "obsidian": obsidian.fetch,
    "youtube": youtube.fetch,
    "transcripts": transcripts.fetch,
    "codex_sessions": codex_sessions.fetch,
}


def main():
    import config

    argv = [a for a in sys.argv[1:] if not a.startswith("-")]
    enabled = [s for s in config.load().get("sources", ["documents"]) if s in CONNECTORS]
    targets = [a for a in argv if a in CONNECTORS] or enabled
    if not targets:
        print("no sources configured. Run setup.py, or add \"sources\" to config.json")
        return

    db = store.connect()
    for name in targets:
        print(f"[{name}] reading...")
        t0 = time.time()
        try:
            docs = CONNECTORS[name]()
        except Exception as e:
            print(f"[{name}] FAILED: {e}")
            continue
        # Long documents are split before storage. A 30-page paper embedded as a
        # single vector describes an average of everything in it, so it matches
        # weakly against almost anything and strongly against nothing.
        pieces = [c for d in docs for c in chunking.chunk_doc(d)]
        if len(pieces) != len(docs):
            print(f"[{name}] {len(docs)} document(s) -> {len(pieces)} passages")
        changed = sum(
            store.upsert(db, d["source"], d["source_id"], d.get("ts"), d.get("title"),
                         d.get("content"), d.get("metadata"), d.get("kind"), d.get("area"))
            for d in pieces
        )
        db.commit()
        print(f"[{name}] {len(pieces)} passages, {changed} new or changed "
              f"({time.time() - t0:.0f}s)")

    print("[embed] vectorising new passages...")
    n = store.embed_missing(db)
    print(f"[embed] {n} vectors written")
    for row in store.stats(db):
        print(f"  {row['source']:14s} {row['n']:6d} passages  {row['embedded']} embedded")
    areas = db.execute(
        "SELECT COALESCE(area,'(none)') a, COUNT(*) n FROM docs GROUP BY a ORDER BY n DESC"
    ).fetchall()
    if len(areas) > 1:
        print("[areas]")
        for r in areas:
            print(f"  {r['a']:14s} {r['n']:6d}")


if __name__ == "__main__":
    main()
