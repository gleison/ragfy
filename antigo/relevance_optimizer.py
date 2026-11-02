#!/usr/bin/env python3
"""Tune the injected chunk count from human verdicts on real prompts.

`chunk_review.py` labels the chunks that Claude Code or Codex actually received.
This module replays those same prompts at a few compact limits, scores the
replayed results against the labels, and writes the best limit to config.json.
It deliberately leaves similarity gates alone: those need a separate, balanced
test set rather than a small sample of live prompts.
"""
from __future__ import annotations

import json
from collections import defaultdict
from pathlib import Path

HERE = Path(__file__).resolve().parent
LIMITS = (3, 4, 6)
MIN_PROMPTS = 3
MIN_LABELS = 8


def labeled_prompts(db):
    """Return real injected prompts that have at least one human verdict."""
    rows = db.execute(
        "SELECT query, queries, doc_id, verdict FROM retrievals "
        "WHERE query IS NOT NULL AND query <> '' AND verdict IS NOT NULL "
        "AND judged_by = 'human' "
        "ORDER BY ts, rank"
    ).fetchall()
    grouped = defaultdict(lambda: {"useful": set(), "junk": set(), "queries": None})
    for row in rows:
        prompt = grouped[row["query"]]
        prompt["queries"] = prompt["queries"] or row["queries"]
        (prompt["useful"] if row["verdict"] else prompt["junk"]).add(row["doc_id"])
    return [
        {"query": query, **labels}
        for query, labels in grouped.items()
        if labels["useful"] or labels["junk"]
    ]


def _queries(value):
    try:
        parsed = json.loads(value or "{}")
    except (TypeError, ValueError):
        parsed = {}
    return parsed if isinstance(parsed, dict) else None


def evaluate(db, prompts, limit):
    """Score a chunk limit against labeled, previously injected chunks."""
    import store

    useful_found = useful_total = judged_returned = judged_useful = 0
    quiet = negative_prompts = returned = 0
    for prompt in prompts:
        results = store.search(
            db, prompt["query"], limit=limit, log=False,
            queries=_queries(prompt["queries"]),
        )
        ids = {row["id"] for _, row in results}
        returned += len(ids)
        useful = prompt["useful"]
        junk = prompt["junk"]
        useful_total += len(useful)
        useful_found += len(ids & useful)
        judged_useful += len(ids & useful)
        judged_returned += len(ids & (useful | junk))
        if not useful:
            negative_prompts += 1
            quiet += int(not ids)
    precision = judged_useful / judged_returned if judged_returned else 0.0
    recall = useful_found / useful_total if useful_total else 1.0
    restraint = quiet / negative_prompts if negative_prompts else 1.0
    average_chunks = returned / max(len(prompts), 1)
    # Relevance wins. Fewer chunks only settles close calls, so an aggressive
    # limit cannot win merely by hiding evidence it would have gotten wrong.
    score = 0.50 * precision + 0.35 * recall + 0.15 * restraint - 0.02 * average_chunks
    return {
        "limit": limit,
        "precision": precision,
        "recall": recall,
        "restraint": restraint,
        "average_chunks": average_chunks,
        "score": score,
    }


def choose(results):
    """Prefer relevance; on ties prefer the smaller context injection."""
    return max(results, key=lambda item: (
        round(item["score"], 8), item["precision"], item["recall"],
        item["restraint"], -item["average_chunks"], -item["limit"],
    ))


def optimize_from_feedback(db, config_path=None):
    """Write the best live-evaluated limit, or explain why more feedback is needed."""
    prompts = labeled_prompts(db)
    labels = sum(len(prompt["useful"]) + len(prompt["junk"]) for prompt in prompts)
    if len(prompts) < MIN_PROMPTS or labels < MIN_LABELS:
        return {
            "optimized": False,
            "message": (
                f"Need {MIN_PROMPTS} reviewed prompts and {MIN_LABELS} reviewed chunks "
                f"before tuning ({len(prompts)} prompts, {labels} chunks so far)."
            ),
        }
    results = [evaluate(db, prompts, limit) for limit in LIMITS]
    best = choose(results)
    path = Path(config_path or HERE / "config.json")
    try:
        cfg = json.loads(path.read_text(encoding="utf-8")) if path.exists() else {}
        cfg.setdefault("retrieval", {})["limit"] = best["limit"]
        path.write_text(json.dumps(cfg, indent=2) + "\n", encoding="utf-8")
    except (OSError, ValueError) as error:
        return {"optimized": False, "message": f"Feedback was saved, but tuning could not update config: {error}"}
    return {
        "optimized": True,
        "limit": best["limit"],
        "metrics": best,
        "message": (
            f"Optimized the recall limit to {best['limit']} chunks from {len(prompts)} reviewed prompts "
            f"({best['precision'] * 100:.0f}% labeled relevance, {best['recall'] * 100:.0f}% useful-chunk recall)."
        ),
    }
