#!/usr/bin/env python3
"""Calibrate retrieval against YOUR documents, then prove it works.

Two thresholds decide everything this system does:

  min_similarity      how close a passage must be to be worth injecting
  min_top_similarity  how close the BEST passage must be before the system
                      answers at all, rather than staying silent

There is no universal right value. They depend on how your material is written,
how long the passages are, and how you phrase questions. Guessing produces one
of two bad outcomes: too low and every prompt gets padded with vaguely-related
noise, too high and real questions get answered with silence.

So this measures instead of guessing, and it does it with GROUND TRUTH rather
than opinion. It samples real passages from your own corpus and asks a model to
write the question each one answers. That gives question-to-passage pairs where
the correct answer is known by construction, which is the part an "ask a model
if this looks relevant" check can never give you. Then it sweeps the thresholds
and reports which settings actually find the known-correct passage, and which
ones stay silent on questions your corpus genuinely cannot answer.

  python autotune.py                 # generate, sweep, write config, write report
  python autotune.py --questions 15
  python autotune.py --dry-run       # measure and report, change nothing
"""
import argparse
import html
import json
import random
import re
import sys
import time
from collections import defaultdict
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import config  # noqa: F401  loads .env
import store

GEN_MODEL = "gpt-4o-mini"
FLOORS = [0.30, 0.35, 0.40, 0.45, 0.50, 0.55]
TOP_FLOORS = [0.35, 0.40, 0.45, 0.50, 0.55, 0.60]
KS = [3, 4, 6]

QGEN = """Below is a passage from someone's personal document library.

Write ONE question that this passage answers, as the OWNER of the library would
actually type it. Real phrasing, not exam phrasing.

Rules:
- Do not quote the passage or reuse its distinctive wording. If the question can
  be answered by string-matching against the passage, it is a bad test.
- Ask about the substance, not about the document ("what does this paper say")
  is useless.
- One sentence. No preamble.

PASSAGE:
{passage}

Reply with only the question."""

NEGGEN = """Someone has a personal document library covering these areas: {areas}.

Write {n} questions this library CANNOT answer. Make them HARD to reject:

- {near} of them must be NEAR MISSES: same broad field, adjacent vocabulary, but
  asking about something the library would not contain. If the areas are medical
  research, a near miss is a question about veterinary dosing, not about baking.
- The rest can be plainly unrelated.

A near miss is the useful test. Anything can reject "what is the capital of
France"; the setting that matters is the one that also rejects a question which
merely SOUNDS like the material.

Reply with ONLY a JSON array of strings."""


def _client():
    from openai import OpenAI

    return OpenAI()


def _ask(prompt, max_tokens=120):
    try:
        return _client().chat.completions.create(
            model=GEN_MODEL, messages=[{"role": "user", "content": prompt}],
            temperature=0.4, max_tokens=max_tokens,
        ).choices[0].message.content.strip()
    except Exception as e:
        print(f"    question generation failed: {str(e)[:90]}")
        return ""


def sample_passages(db, n, min_chars=400):
    """Spread the sample across areas so one big folder does not define the tuning."""
    rows = db.execute(
        "SELECT id, area, title, content, metadata, source_id FROM docs "
        "WHERE embedding IS NOT NULL AND LENGTH(content) >= ?", (min_chars,)).fetchall()
    if not rows:
        return []
    by_area = defaultdict(list)
    for r in rows:
        by_area[r["area"] or "(none)"].append(r)
    rng = random.Random(11)          # fixed seed: a re-run is comparable
    for v in by_area.values():
        rng.shuffle(v)
    picked, i = [], 0
    while len(picked) < n and any(len(v) > i for v in by_area.values()):
        for v in by_area.values():
            if len(v) > i and len(picked) < n:
                picked.append(v[i])
        i += 1
    return picked


def _parent_of(row):
    """The document a passage came from, or the passage itself if it was never split."""
    try:
        return (json.loads(row["metadata"] or "{}").get("parent")
                or row["source_id"] or row["id"])
    except Exception:
        return row["id"]


def build_gold(db, n):
    """[(question, correct_doc_id, area, title)] with the answer known up front."""
    passages = sample_passages(db, n)
    if not passages:
        print("no documents with enough text to tune on — ingest something first")
        return []
    gold = []
    print(f"writing {len(passages)} test questions from your own documents…")
    for i, row in enumerate(passages, 1):
        q = _ask(QGEN.format(passage=(row["content"] or "")[:2200]))
        q = q.strip().strip('"')
        if len(q) < 12 or "\n" in q[:12]:
            continue
        gold.append({"q": q, "doc_id": row["id"], "area": row["area"] or "",
                     "title": row["title"] or "", "parent": _parent_of(row)})
        print(f"  {i:2d}/{len(passages)}  {q[:88]}")
    return gold


def build_negatives(db, n=5):
    areas = [r[0] for r in db.execute(
        "SELECT DISTINCT COALESCE(area,'general') FROM docs") if r[0]]
    out = _ask(NEGGEN.format(areas=", ".join(areas[:8]) or "general notes",
                             n=n, near=max(1, n * 2 // 3)), 500)
    try:
        s, e = out.find("["), out.rfind("]")
        qs = json.loads(out[s:e + 1])
        return [q for q in qs if isinstance(q, str)][:n]
    except Exception:
        return ["What is the boiling point of nitrogen at two atmospheres?",
                "Who won the 1994 FIFA World Cup and what was the score?",
                "Give me a recipe for sourdough starter from scratch."][:n]


def distill_all(questions):
    """Distil every test question ONCE.

    The sweep runs each question against a hundred-odd threshold combinations.
    Distilling inside the loop would cost a hundred model calls per question and,
    worse, let a differently-worded rewrite change the result between two
    settings that are supposed to differ only in their thresholds."""
    cache = {}
    cfg = config.load().get("query_distill", {})
    for q in questions:
        try:
            cache[q] = store.distill_query(q, cfg) or {"queries": [q], "dates": [], "tense": None}
        except Exception:
            cache[q] = {"queries": [q], "dates": [], "tense": None}
    return cache


def evaluate(db, gold, negatives, floor, top_floor, k, qcache):
    """Recall on questions with a known answer; silence on questions without one."""
    hits = doc_hits = same_doc = total_chunks = 0
    rank_sum = 0.0
    for g in gold:
        res = store.search(db, g["q"], limit=k, floor=floor,
                           min_top=top_floor, queries=qcache.get(g["q"]))
        ids = [row["id"] for _, row in res]
        parents = {_parent_of(row) for _, row in res}
        total_chunks += len(ids)
        if g["doc_id"] in ids:
            hits += 1
            rank_sum += 1.0 / (ids.index(g["doc_id"]) + 1)
        # Passage ids change every time the chunk size changes, so exact-passage
        # recall cannot compare one chunking strategy against another: a re-chunk
        # produces a different set of passages and therefore a different exam.
        # Recall at the DOCUMENT level survives re-chunking and is the number to
        # watch when deciding how to split material up.
        if g.get("parent") in parents:
            doc_hits += 1
        same_doc += sum(1 for i in ids if i == g["doc_id"])
    quiet = 0
    for q in negatives:
        res = store.search(db, q, limit=k, floor=floor, min_top=top_floor,
                           queries=qcache.get(q))
        quiet += (len(res) == 0)
    n = max(len(gold), 1)
    return {
        "floor": floor, "top": top_floor, "k": k,
        "recall": hits / n,                       # found the known-correct passage
        "doc_recall": doc_hits / n,               # found its document, comparable across chunkings
        "mrr": rank_sum / n,                      # and how near the top it was
        "restraint": quiet / max(len(negatives), 1),
        "avg_chunks": total_chunks / n,
        "precision_proxy": same_doc / max(total_chunks, 1),
    }


def score(m, target_chunks=6):
    """Recall is the point. Restraint is non-negotiable. Volume is a tiebreak.

    Weighted so a config that answers everything by never filtering cannot win:
    restraint carries as much as recall, and returning far more chunks than
    asked for is penalised rather than banned.

    Note on a real limitation: the test questions are written FROM passages, so
    they are cleaner and more on-topic than anything a person types at 11pm. That
    biases the sweep toward permissive settings. The near-miss negatives and this
    volume penalty push back on it, and ties break toward the stricter setting,
    but treat the result as a good starting point rather than a final answer.
    Re-run it once there is real usage to check it against."""
    over = max(0.0, m["avg_chunks"] - target_chunks) / target_chunks
    return (0.45 * m["recall"] + 0.20 * m["mrr"] + 0.30 * m["restraint"]
            + 0.05 * m["precision_proxy"] - 0.25 * over)


def _strictness(m):
    """Tie-break key. Equal scores mean the thresholds never separated on these
    questions, and the stricter setting is the safer thing to ship: it fails by
    staying quiet rather than by inventing relevance."""
    return (round(score(m), 4), m["floor"], m["top"], -m["k"])


def report_html(gold, negatives, results, best, db, path):
    css = """body{max-width:900px;margin:0 auto;padding:30px 20px 70px;
    font:15px/1.6 ui-sans-serif,-apple-system,Segoe UI,sans-serif;background:#fbfbfa;color:#1c1b19}
    @media(prefers-color-scheme:dark){body{background:#161513;color:#eceae5}
      table{border-color:#2e2c28}td,th{border-color:#2e2c28}pre{background:#232120}}
    h1{font-size:21px;margin:0 0 4px}h2{font-size:16px;margin:30px 0 8px}
    .s{color:#6b6862;font-size:13px}table{border-collapse:collapse;width:100%;font-size:13px;margin:8px 0}
    td,th{border:1px solid #e3e0da;padding:6px 9px;text-align:left}th{font-weight:600}
    tr.best{background:#e7f5ed;font-weight:600}
    @media(prefers-color-scheme:dark){tr.best{background:#14301f}}
    pre{background:#f4f2ee;padding:9px 12px;border-radius:6px;white-space:pre-wrap;
        font:12px/1.5 ui-monospace,Menlo,monospace;margin:4px 0}
    .ok{color:#1a7f4b;font-weight:600}.no{color:#b3261e;font-weight:600}"""
    o = [f"<style>{css}</style>", "<h1>Retrieval calibration report</h1>",
         f"<div class=s>{len(gold)} questions written from your own documents, "
         f"{len(negatives)} questions your library should not answer, "
         f"{len(results)} settings combinations tested.</div>",
         "<h2>Chosen settings</h2>",
         f"<pre>min_similarity      {best['floor']}\nmin_top_similarity  {best['top']}\n"
         f"limit               {best['k']}</pre>",
         f"<div class=s>Found the correct passage for "
         f"<b>{best['recall']*100:.0f}%</b> of answerable questions, and stayed silent on "
         f"<b>{best['restraint']*100:.0f}%</b> of unanswerable ones, "
         f"returning {best['avg_chunks']:.1f} chunks on average.</div>",
         "<h2>What each setting scored</h2>",
         "<table><tr><th>min_sim</th><th>min_top</th><th>k</th><th>found the passage</th>"
         "<th>found the document</th><th>near the top</th><th>stayed quiet</th>"
         "<th>avg chunks</th><th>score</th></tr>"]
    for m in sorted(results, key=_strictness, reverse=True)[:14]:
        cls = " class=best" if m is best else ""
        o.append(f"<tr{cls}><td>{m['floor']}</td><td>{m['top']}</td><td>{m['k']}</td>"
                 f"<td>{m['recall']*100:.0f}%</td><td>{m['doc_recall']*100:.0f}%</td>"
                 f"<td>{m['mrr']:.2f}</td>"
                 f"<td>{m['restraint']*100:.0f}%</td><td>{m['avg_chunks']:.1f}</td>"
                 f"<td>{score(m):.3f}</td></tr>")
    o.append("</table>")

    o.append("<h2>Every question, and what came back</h2>")
    o.append("<div class=s>The passage each question was written from is the correct "
             "answer. A tick means the system found it.</div>")
    for g in gold:
        res = store.search(db, g["q"], limit=best["k"], floor=best["floor"],
                           min_top=best["top"])
        ids = [row["id"] for _, row in res]
        found = g["doc_id"] in ids
        pos = ids.index(g["doc_id"]) + 1 if found else 0
        o.append(f"<h2 style='font-size:14px'>{html.escape(g['q'])}</h2>")
        mark = (f"<span class=ok>found at position {pos}</span>" if found
                else "<span class=no>did not find it</span>")
        o.append(f"<div class=s>expected: {html.escape(g['title'][:90])} &middot; {mark} "
                 f"&middot; {len(res)} chunk(s) returned</div>")
        for _, row in res[:4]:
            tag = " &#10003;" if row["id"] == g["doc_id"] else ""
            body = " ".join((row["content"] or "").split())[:240]
            o.append(f"<pre>[{html.escape(row['area'] or row['source'])}]{tag} "
                     f"{html.escape(row['title'] or '')}\n{html.escape(body)}</pre>")
    o.append("<h2>Questions it should refuse</h2>")
    for q in negatives:
        res = store.search(db, q, limit=best["k"], floor=best["floor"],
                           min_top=best["top"])
        v = ("<span class=ok>returned nothing, correct</span>" if not res
             else f"<span class=no>returned {len(res)} chunk(s)</span>")
        o.append(f"<div style='margin:6px 0'>{html.escape(q)} &mdash; {v}</div>")
    Path(path).write_text("\n".join(o))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--questions", type=int, default=12)
    ap.add_argument("--negatives", type=int, default=5)
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--report", default="calibration-report.html")
    args = ap.parse_args()

    db = store.connect()
    n_docs = db.execute("SELECT COUNT(*) FROM docs WHERE embedding IS NOT NULL").fetchone()[0]
    print(f"corpus: {n_docs} embedded passages\n")
    if n_docs < 20:
        print("Not enough material to calibrate against. Run setup.py and ingest first.")
        return

    gold = build_gold(db, args.questions)
    if not gold:
        return
    print(f"\nwriting {args.negatives} questions your library should NOT answer…")
    negatives = build_negatives(db, args.negatives)
    for q in negatives:
        print(f"  {q[:88]}")

    qcache = distill_all([g["q"] for g in gold] + negatives)
    combos = [(f, t, k) for f in FLOORS for t in TOP_FLOORS for k in KS if t >= f]
    print(f"\ntesting {len(combos)} settings combinations "
          f"({len(gold)} + {len(negatives)} questions each)…")
    t0 = time.time()
    results = []
    for i, (f, t, k) in enumerate(combos, 1):
        results.append(evaluate(db, gold, negatives, f, t, k, qcache))
        if i % 20 == 0:
            print(f"  {i}/{len(combos)}  ({time.time()-t0:.0f}s)")
    best = max(results, key=_strictness)

    print(f"\n{'min_sim':>8} {'min_top':>8} {'k':>3} {'passage':>8} {'doc':>6} "
          f"{'quiet':>7} {'chunks':>7} {'score':>7}")
    for m in sorted(results, key=_strictness, reverse=True)[:8]:
        star = " <-" if m is best else ""
        print(f"{m['floor']:8.2f} {m['top']:8.2f} {m['k']:3d} {m['recall']*100:7.0f}% "
              f"{m['doc_recall']*100:5.0f}% {m['restraint']*100:6.0f}% "
              f"{m['avg_chunks']:7.1f} {score(m):7.3f}{star}")

    print(f"\nchosen: min_similarity={best['floor']}  min_top_similarity={best['top']}  limit={best['k']}")
    print(f"  finds the correct passage for {best['recall']*100:.0f}% of answerable questions")
    print(f"  finds its source document for {best['doc_recall']*100:.0f}% "
          f"(this is the number to compare when changing chunk size)")
    print(f"  stays silent on {best['restraint']*100:.0f}% of unanswerable ones")

    report_html(gold, negatives, results, best, db, args.report)
    print(f"\nwrote {args.report} — open it to read every question and what came back")

    if args.dry_run:
        print("(dry run, config unchanged)")
        return
    cfg_path = HERE / "config.json"
    cfg = json.loads(cfg_path.read_text()) if cfg_path.exists() else {}
    cfg.setdefault("retrieval", {}).update({
        "min_similarity": best["floor"],
        "min_top_similarity": best["top"],
        "limit": best["k"],
    })
    cfg_path.write_text(json.dumps(cfg, indent=2) + "\n")
    print(f"wrote settings into {cfg_path.name}")


if __name__ == "__main__":
    main()
