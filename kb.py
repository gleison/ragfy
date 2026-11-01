"""NSM Knowledge Base CLI.

  python kb.py search "query" [--source X] [--limit N]
  python kb.py ask "question"        # retrieval + LLM synthesis with citations
  python kb.py add "title" --content "..." [--source intake] [--url U]
  python kb.py stats
"""
import argparse
import hashlib
import json
import sys
from datetime import datetime, timezone
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
import config  # noqa: F401 — loads .env into the environment
import store

SYNTH_MODEL = None

SYNTH_PROMPT = """You are this team's internal knowledge base. Answer the question using ONLY the evidence below. Cite sources inline like [slack:3] using the bracketed ids. If sources conflict, prefer the most recent evidence (dates are shown). If the evidence doesn't answer it, say so.

Question: {question}

Evidence:
{evidence}"""


def cmd_search(args):
    db = store.connect()
    results = store.search(db, args.query, limit=args.limit, source=args.source)
    for score, row in results:
        meta = json.loads(row["metadata"] or "{}")
        loc = meta.get("channel") or meta.get("account") or meta.get("repo") or meta.get("url") or ""
        print(f"[{row['source']}] {score:.4f}  {(row['ts'] or '')[:10]}  {loc}")
        print(f"  {row['title']}")
        snippet = " ".join((row["content"] or "").split())[:200]
        print(f"  {snippet}\n")


def cmd_ask(args):
    import llm

    db = store.connect()
    results = store.search(db, args.query, limit=8)
    evidence = []
    for i, (_, row) in enumerate(results):
        body = " ".join((row["content"] or "").split())[:1500]
        evidence.append(f"[{row['source']}:{i}] ({(row['ts'] or '')[:10]}) {row['title']}\n{body}")
    print(llm.complete(SYNTH_PROMPT.format(question=args.query, evidence="\n\n".join(evidence)), SYNTH_MODEL, 1000))
    print("\n--- sources ---")
    for i, (_, row) in enumerate(results):
        print(f"[{row['source']}:{i}] {row['title']}")


def cmd_add(args):
    """Push one doc directly (the /intake lane): upserted + embedded immediately."""
    db = store.connect()
    content = args.content or (Path(args.file).read_text() if args.file else sys.stdin.read())
    sid = args.id or hashlib.sha1((args.title + (args.url or "")).encode()).hexdigest()[:12]
    ts = args.ts or datetime.now(timezone.utc).isoformat()
    changed = store.upsert(db, args.source, sid, ts, args.title, content, {"url": args.url or ""})
    db.commit()
    store.embed_missing(db)
    print(f"{'added' if changed else 'unchanged'}: [{args.source}] {args.title} ({sid})")


def cmd_stats(args):
    db = store.connect()
    for row in store.stats(db):
        print(f"{row['source']:8s} {row['n']:5d} docs  {row['embedded']} embedded  newest {(row['newest'] or '')[:10]}")


def main():
    p = argparse.ArgumentParser()
    sub = p.add_subparsers(dest="cmd", required=True)
    s = sub.add_parser("search")
    s.add_argument("query")
    s.add_argument("--source")
    s.add_argument("--limit", type=int, default=10)
    a = sub.add_parser("ask")
    a.add_argument("query")
    d = sub.add_parser("add")
    d.add_argument("title")
    d.add_argument("--content")
    d.add_argument("--file")
    d.add_argument("--source", default="intake")
    d.add_argument("--url")
    d.add_argument("--id")
    d.add_argument("--ts")
    sub.add_parser("stats")
    args = p.parse_args()
    {"search": cmd_search, "ask": cmd_ask, "add": cmd_add, "stats": cmd_stats}[args.cmd](args)


if __name__ == "__main__":
    main()
