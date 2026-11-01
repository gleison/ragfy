#!/usr/bin/env python3
"""Corpus curator — the thing that stops a knowledge base rotting into a swamp.

Retrieval quality is not only about what goes in; a KB that only ever grows
gets worse every day, because stale records stay semantically relevant to the
questions they used to answer. Three jobs, cheapest first:

  1. SUPERSEDE  Two facts that say nearly the same thing are almost never both
                worth keeping — the newer one has usually replaced the older
                ("channel has 21 subscribers" -> "channel has 400"). Vector
                similarity finds the pairs; the newer wins. Only `fact` and
                `decision` supersede by default: a second write-up of the same
                PROBLEM is often a genuinely different failure with the same
                smell, and killing it loses a real answer.

  3. CHECKPOINT A record survives by being USED, not by being young. At each
                age checkpoint a document must show it has been retrieved
                inside the relevant window; if it has not, it is deleted. So
                everything stays hot through its first three weeks (a lot
                changes in a month, and a new record has had no chance to prove
                itself), then has to keep earning its place. Something from
                three years ago that still fires weekly outlives something
                written last month that never fired once.

                A checkpoint is only applied when the document has been
                OBSERVABLE for the whole window. Retrieval logging started on a
                specific date; nothing is judged on evidence that could not
                have existed yet, or the first run would delete the corpus.

  2. AUDIT      (--audit) The only non-circular quality signal in the system.
                Counting retrievals cannot tell a useful record from one that
                keeps getting served by mistake, because retrieval quality is
                exactly what is in question. So a judge reads real logged
                queries next to the records that were served for them and marks
                each 1 or 0. Those verdicts gate usage credit, and a record the
                judge keeps rejecting is deleted no matter how often it was
                pulled. This is what stops junk ratcheting itself to the top.

                (--llm) also reviews the ambiguous band below the supersede cut.

  python curate.py --dry-run
  python curate.py --apply
  python curate.py --apply --llm
"""
import argparse
import json
import sys
from collections import defaultdict
from datetime import datetime, timedelta, timezone
from pathlib import Path

import numpy as np

sys.path.insert(0, str(Path(__file__).parent))
import config  # noqa: F401
import store

SUPERSEDE_KINDS = ("fact", "decision")
REVIEW_BAND = 0.07  # width of the "ask a model" band below the supersede cut

AUDIT_PROMPT = """A knowledge base injected these records as "relevant context" for the
prompt below. Mark each one 1 if it is genuinely relevant to that prompt, or 0 if it is
noise that would only waste context. Be strict: sharing a topic area is not enough, and
keyword overlap alone is 0.

PROMPT: {query}

RECORDS:
{records}

Reply with ONLY a JSON array of 0/1 integers, one per record, in order. Nothing else."""

VERDICT_PROMPT = """Two records from a personal knowledge base. Does the NEWER one
make the OLDER one obsolete — same subject, updated answer? Or are they independent
facts that happen to be worded similarly?

OLDER ({old_ts}): {old}

NEWER ({new_ts}): {new}

Reply with exactly one word: SUPERSEDES or INDEPENDENT."""


def _cfg():
    return config.load().get("curator", {})


def _load(db):
    rows = db.execute(
        "SELECT id, kind, ts, title, content, embedding FROM docs WHERE embedding IS NOT NULL"
    ).fetchall()
    mat = np.stack([np.frombuffer(r["embedding"], dtype=np.float32) for r in rows]) if rows else np.zeros((0, 1536), np.float32)
    if len(rows):
        mat /= np.linalg.norm(mat, axis=1, keepdims=True) + 1e-9
    return rows, mat


def _retrieval_counts(db):
    try:
        return {r["doc_id"]: r["n"] for r in db.execute(
            "SELECT doc_id, COUNT(*) AS n FROM retrievals GROUP BY doc_id")}
    except Exception:
        return {}


def _ts(row):
    return row["ts"] or ""


def find_supersessions(rows, mat, cut, band=0.0, kinds=SUPERSEDE_KINDS):
    """Pairs (loser_id, winner_id, similarity) plus the ambiguous ones."""
    idx = [i for i, r in enumerate(rows) if (r["kind"] or "") in kinds]
    kill, review = [], []
    if len(idx) < 2:
        return kill, review
    sub = mat[idx]
    sims = sub @ sub.T
    np.fill_diagonal(sims, -1.0)
    dead = set()
    for a in range(len(idx)):
        if idx[a] in dead:
            continue
        for b in np.where(sims[a] >= cut - band)[0]:
            if b <= a or idx[b] in dead:
                continue
            ra, rb = rows[idx[a]], rows[idx[b]]
            older, newer = (ra, rb) if _ts(ra) <= _ts(rb) else (rb, ra)
            sim = float(sims[a][b])
            if sim >= cut:
                kill.append((older["id"], newer["id"], sim))
                dead.add(older["id"])
            elif band:
                review.append((older, newer, sim))
    return kill, review


def run_audit(db, byid_all, sample, kill_ratio, apply):
    """Judge real retrievals, write verdicts, return doc ids that keep failing."""
    import llm

    try:
        pend = db.execute(
            """SELECT rowid, doc_id, query FROM retrievals
               WHERE verdict IS NULL AND query IS NOT NULL AND query <> ''
               ORDER BY ts DESC LIMIT ?""", (sample * 8,)).fetchall()
    except Exception:
        print("\n2. AUDIT: no retrieval log yet")
        return []
    groups = defaultdict(list)
    for r in pend:
        groups[r["query"]].append((r["rowid"], r["doc_id"]))
    queries = list(groups.items())[:sample]
    if not queries:
        print("\n2. AUDIT: nothing new to judge")
        return []

    graded = 0
    for query, items in queries:
        recs = []
        for i, (_rid, did) in enumerate(items, 1):
            row = byid_all.get(did)
            if not row:
                continue
            body = " ".join((row["content"] or "").split())[:220]
            recs.append(f"[{i}] ({row['kind'] or '-'}) {row['title']}\n    {body}")
        if not recs:
            continue
        try:
            out = llm.complete(AUDIT_PROMPT.format(query=query[:400],
                                                   records="\n".join(recs)),
                               None, 200).strip()
            start, end = out.find("["), out.rfind("]")
            verdicts = json.loads(out[start:end + 1])
        except Exception:
            continue
        for (rid, _did), v in zip(items, verdicts):
            # Never touch a row a human already judged.
            db.execute("UPDATE retrievals SET verdict=?, judged_by='llm' WHERE rowid=? "
                       "AND COALESCE(judged_by,'') <> 'human'",
                       (1 if int(v) else 0, rid))
        graded += 1
    db.commit()

    # A person calling a chunk junk is decisive. A model needs corroboration:
    # it shares blind spots with the retriever it is grading.
    human_bad = [r["doc_id"] for r in db.execute(
        """SELECT doc_id FROM retrievals WHERE judged_by='human' AND verdict=0
           GROUP BY doc_id""")]
    stats = db.execute(
        """SELECT doc_id, COUNT(*) AS n, SUM(verdict = 0) AS bad FROM retrievals
           WHERE verdict IS NOT NULL AND COALESCE(judged_by,'llm')='llm'
           GROUP BY doc_id HAVING n >= 2""").fetchall()
    losers = [r["doc_id"] for r in stats if r["bad"] / r["n"] > kill_ratio]
    kept_by_human = {r["doc_id"] for r in db.execute(
        "SELECT doc_id FROM retrievals WHERE judged_by='human' AND verdict=1")}
    losers = [d for d in losers if d not in kept_by_human] + \
             [d for d in human_bad if d not in kept_by_human]
    losers = sorted(set(losers))
    if human_bad:
        print(f"   {len(human_bad)} rejected by Mason directly (decisive)")
    print(f"\n2. AUDIT: judged {graded} real queries")
    print(f"   records rejected in >{kill_ratio:.0%} of their appearances: {len(losers)}")
    for d in losers[:6]:
        row = byid_all.get(d)
        if row:
            print(f"   DROP [{row['kind'] or '-'}] {row['title'][:60]}")
    return losers


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--apply", action="store_true")
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--llm", action="store_true", help="review the ambiguous band with a model")
    ap.add_argument("--audit", action="store_true",
                    help="judge real logged retrievals for relevance; the only "
                         "signal that is not circular")
    args = ap.parse_args()
    if not (args.apply or args.dry_run):
        ap.error("pass --dry-run or --apply")

    cfg = _cfg()
    cut = float(cfg.get("supersede_similarity", 0.95))

    db = store.connect()
    rows, mat = _load(db)
    counts = _retrieval_counts(db)
    by_kind = defaultdict(int)
    for r in rows:
        by_kind[r["kind"] or "(untyped)"] += 1
    print(f"corpus: {len(rows)} embedded docs  {dict(by_kind)}")
    print(f"retrieval log: {len(counts)} docs have ever been served")

    audited_out = []
    if args.audit:
        audited_out = run_audit(db, byid_all={r["id"]: r for r in rows},
                                sample=int(cfg.get("audit_sample", 60)),
                                kill_ratio=float(cfg.get("audit_kill_ratio", 0.5)),
                                apply=args.apply)

    kill, review = find_supersessions(rows, mat, cut, REVIEW_BAND if args.llm else 0.0)
    print(f"\n1. SUPERSEDED (>= {cut} similar, older loses): {len(kill)}")
    byid = {r["id"]: r for r in rows}
    for old, new, sim in kill[:8]:
        print(f"   {sim:.3f}  DROP {byid[old]['title'][:58]}")
        print(f"           KEEP {byid[new]['title'][:58]}")

    if args.llm and review:
        import llm
        print(f"\n2. AMBIGUOUS BAND ({cut-REVIEW_BAND:.2f}-{cut:.2f}): {len(review)} pairs, asking the model")
        for older, newer, sim in review:
            try:
                v = llm.complete(VERDICT_PROMPT.format(
                    old_ts=_ts(older)[:10], old=older["content"][:600],
                    new_ts=_ts(newer)[:10], new=newer["content"][:600]),
                    None, 10).strip().upper()
            except Exception:
                continue
            if v.startswith("SUPERSEDES"):
                kill.append((older["id"], newer["id"], sim))
                print(f"   {sim:.3f}  model says SUPERSEDES: {older['title'][:52]}")

    life = config.load().get("lifecycle", {})
    now = datetime.now(timezone.utc)
    try:
        epoch = datetime.fromisoformat(life.get("logging_started", "2026-08-21"))
        if epoch.tzinfo is None:
            epoch = epoch.replace(tzinfo=timezone.utc)
    except ValueError:
        epoch = now
    grace = float(life.get("grace_days", 21))
    protected = set(life.get("protected_kinds", []))
    leash = life.get("leash_multiplier", {})
    checkpoints = sorted(life.get("checkpoints", []), key=lambda c: c["age_days"])
    last_use = {}
    try:
        for r in db.execute("SELECT doc_id, MAX(ts) AS last FROM retrievals GROUP BY doc_id"):
            last_use[r["doc_id"]] = r["last"]
    except Exception:
        pass

    def _days(ts):
        try:
            d = datetime.fromisoformat((ts or "").replace("Z", "+00:00"))
            if d.tzinfo is None:
                d = d.replace(tzinfo=timezone.utc)
            return (now - d).total_seconds() / 86400
        except ValueError:
            return None

    expired, held_back = [], 0
    for r in rows:
        kind = r["kind"] or ""
        if kind in protected:
            continue
        age = _days(_ts(r))
        if age is None or age < grace:
            continue
        mult = float(leash.get(kind, 1.0))
        # Which checkpoint is this doc at? The strictest one it has passed.
        cp = None
        for c in checkpoints:
            if age >= float(c["age_days"]) * mult:
                cp = c
        if cp is None:
            continue
        window = float(cp["used_within_days"]) * mult
        # Observability: the doc must have existed AND logging must have been on
        # for the whole window, or absence of use proves nothing.
        observable = min(age, (now - epoch).total_seconds() / 86400)
        if observable < window:
            held_back += 1
            continue
        used = _days(last_use.get(r["id"]))
        if used is not None and used <= window:
            continue                      # earned its place
        if used is None and counts.get(r["id"], 0) >= int(cp.get("min_uses", 1)):
            continue                      # used, timestamp unreadable — keep
        expired.append(r["id"])

    print(f"\n3. CHECKPOINT EXPIRY (unused past an age checkpoint): {len(expired)}")
    if held_back:
        print(f"   {held_back} doc(s) not yet judgeable — retrieval logging began "
              f"{(now-epoch).days}d ago and their window is not complete")
    for i in expired[:6]:
        a = _days(_ts(byid[i]))
        print(f"   age {a:5.0f}d  [{byid[i]['kind'] or '-'}]  {byid[i]['title'][:56]}")

    doomed = {old for old, _, _ in kill} | set(expired) | set(audited_out)
    print(f"\nTOTAL to remove: {len(doomed)} of {len(rows)} ({100*len(doomed)/max(len(rows),1):.1f}%)")
    if args.dry_run:
        print("(dry run — nothing deleted)")
        return
    db.executemany("DELETE FROM docs WHERE id=?", [(i,) for i in doomed])
    db.execute("DELETE FROM retrievals WHERE doc_id NOT IN (SELECT id FROM docs)")
    db.commit()
    db.execute("INSERT INTO docs_fts(docs_fts) VALUES('rebuild')")
    db.commit()
    print(f"removed {len(doomed)}; corpus now "
          f"{db.execute('SELECT COUNT(*) FROM docs').fetchone()[0]} docs")


if __name__ == "__main__":
    main()
