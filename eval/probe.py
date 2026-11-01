#!/usr/bin/env python3
"""Show exactly what the recall hook would inject, as a readable HTML page.

  python eval/probe.py --questions my_questions.txt --out probe.html
  python eval/probe.py --q "what LUT do I use" --q "when did I meet Alex"

One question per line in the file. Runs the REAL search path (same config, same
floors, same segmentation) so what you read is what a prompt would actually get.
Every retrieved chunk is shown in full with its similarity, so a chunk can be
judged on its own merits rather than on a truncated preview.
"""
import argparse, html, json, sys, time
from pathlib import Path

KB = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(KB))
import config  # noqa: F401
import store

CSS = """
:root{--bg:#fbfbfa;--fg:#1c1b19;--mut:#6b6862;--line:#e3e0da;--card:#fff;
      --good:#1a7f4b;--mid:#a8720e;--bad:#b3261e;--code:#f4f2ee}
@media(prefers-color-scheme:dark){:root{--bg:#161513;--fg:#eceae5;--mut:#9a958c;
      --line:#2e2c28;--card:#1e1d1a;--good:#4ade80;--mid:#fbbf24;--bad:#f87171;--code:#232120}}
*{box-sizing:border-box}
body{margin:0;padding:32px 20px 80px;background:var(--bg);color:var(--fg);
     font:15px/1.6 ui-sans-serif,-apple-system,"SF Pro Text",Segoe UI,sans-serif}
.wrap{max-width:920px;margin:0 auto}
h1{font-size:22px;margin:0 0 4px;letter-spacing:-.01em}
.sub{color:var(--mut);font-size:13px;margin-bottom:28px}
.q{background:var(--card);border:1px solid var(--line);border-radius:10px;
   padding:18px 20px;margin-bottom:18px}
.qh{display:flex;gap:10px;align-items:baseline;margin-bottom:4px}
.qn{font:600 12px/1 ui-monospace,SFMono-Regular,Menlo,monospace;color:var(--mut);
    border:1px solid var(--line);border-radius:5px;padding:4px 7px}
.qt{font-weight:600;font-size:16px}
.meta{color:var(--mut);font-size:12.5px;margin:6px 0 14px}
.none{color:var(--bad);font-weight:600;font-size:13.5px;padding:10px 0}
.c{border-top:1px solid var(--line);padding:13px 0 3px}
.ch{display:flex;gap:9px;align-items:baseline;flex-wrap:wrap;margin-bottom:5px}
.k{font:600 10.5px/1 ui-monospace,Menlo,monospace;text-transform:uppercase;
   letter-spacing:.05em;padding:3px 6px;border-radius:4px;background:var(--code);color:var(--mut)}
.sim{font:600 12px/1 ui-monospace,Menlo,monospace}
.hi{color:var(--good)}.md{color:var(--mid)}.lo{color:var(--bad)}
.dt{color:var(--mut);font-size:11.5px;font-family:ui-monospace,Menlo,monospace}
.ct{font-weight:600;font-size:14px;margin-bottom:5px}
pre{background:var(--code);border-radius:7px;padding:11px 13px;margin:0;
    font:12.5px/1.55 ui-monospace,SFMono-Regular,Menlo,monospace;
    white-space:pre-wrap;word-break:break-word;overflow-x:auto}
.seg{margin:0 0 14px;padding:9px 12px;background:var(--code);border-radius:7px;
     font-size:12.5px;color:var(--mut)}
.seg b{color:var(--fg);font-weight:600}
.seg ul{margin:5px 0 0;padding-left:18px}
"""


def band(s):
    return "hi" if s >= 0.55 else ("md" if s >= 0.48 else "lo")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--questions")
    ap.add_argument("--q", action="append", default=[])
    ap.add_argument("--out", default="probe.html")
    ap.add_argument("--limit", type=int)
    args = ap.parse_args()

    qs = list(args.q)
    if args.questions:
        qs += [l.strip() for l in Path(args.questions).read_text().splitlines()
               if l.strip() and not l.startswith("#")]
    if not qs:
        ap.error("give --q or --questions")

    cfg = config.load().get("retrieval", {})
    limit = args.limit or int(cfg.get("limit", 6))
    db = store.connect()
    total = db.execute("SELECT COUNT(*) FROM docs").fetchone()[0]

    out = [f"<style>{CSS}</style><div class=wrap>",
           "<h1>What the knowledge base would inject</h1>",
           f"<div class=sub>{total} records &middot; floor {cfg.get('min_similarity')} per chunk, "
           f"{cfg.get('min_top_similarity')} to answer at all &middot; max {limit} chunks &middot; "
           "green &ge;0.55 &middot; amber &ge;0.48 &middot; red below</div>"]

    empty = 0
    for i, q in enumerate(qs, 1):
        t0 = time.time()
        hits = store.search(db, q, limit=limit)
        ms = int((time.time() - t0) * 1000)
        segs = store._segments(q)
        out.append("<div class=q>")
        out.append(f"<div class=qh><span class=qn>Q{i:02d}</span>"
                   f"<span class=qt>{html.escape(q)}</span></div>")
        out.append(f"<div class=meta>{len(hits)} chunk(s) &middot; {ms} ms</div>")
        if len(segs) > 2:
            items = "".join(f"<li>{html.escape(s[:130])}</li>" for s in segs[:-1])
            out.append(f"<div class=seg><b>Searched as {len(segs)-1} separate thoughts:</b>"
                       f"<ul>{items}</ul></div>")
        if not hits:
            empty += 1
            out.append("<div class=none>Nothing cleared the floor. Nothing injected.</div>")
        for score, row in hits:
            sim = store._last_sims.get(row["id"], 0.0) if hasattr(store, "_last_sims") else 0.0
            out.append("<div class=c>")
            out.append(f"<div class=ch><span class=k>{html.escape(row['kind'] or row['source'])}</span>"
                       f"<span class='sim {band(sim)}'>sim {sim:.3f}</span>"
                       f"<span class=dt>rank {score:.5f} &middot; {(row['ts'] or '')[:10]}</span></div>")
            out.append(f"<div class=ct>{html.escape(row['title'] or '')}</div>")
            out.append(f"<pre>{html.escape(row['content'] or '')}</pre></div>")
        out.append("</div>")

    out.append(f"<div class=sub>{len(qs)} questions &middot; {empty} returned nothing</div></div>")
    Path(args.out).write_text("\n".join(out))
    print(f"wrote {args.out}  ({len(qs)} questions, {empty} empty)")


if __name__ == "__main__":
    main()
