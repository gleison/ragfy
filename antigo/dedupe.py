#!/usr/bin/env python3
"""One-shot corpus cleanup + a guard so the mess can't come back.

Claude Code writes a NEW .jsonl every time a session is resumed, containing the
whole prior conversation. The transcript distiller saw each of those as a new
source_id and re-distilled the same work, so ~36% of the corpus became exact
duplicate rows. Same-content rows are collapsed to the oldest id (which keeps
the earliest real timestamp) and the raw-spine fallback docs — sessions whose
distill failed and got stored as an undigested transcript head — are deleted.

  python dedupe.py --dry-run
  python dedupe.py --apply
"""
import argparse, hashlib, sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
import store


def report(db):
    total = db.execute("SELECT COUNT(*) FROM docs").fetchone()[0]
    dup_rows = db.execute(
        "SELECT COALESCE(SUM(c-1),0) FROM (SELECT COUNT(*) c FROM docs GROUP BY source, content HAVING c>1)"
    ).fetchone()[0]
    spine = db.execute(
        "SELECT COUNT(*) FROM docs WHERE source='transcripts' AND title LIKE 'Session in %'"
    ).fetchone()[0]
    thin = db.execute(
        "SELECT COUNT(*) FROM docs WHERE LENGTH(TRIM(COALESCE(content,'')))<80"
    ).fetchone()[0]
    return total, dup_rows, spine, thin


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--apply", action="store_true")
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--keep-thin", action="store_true",
                    help="keep sub-80-char docs (bare calendar entries etc.)")
    ap.add_argument("--purge-disabled", action="store_true",
                    help="delete rows from sources no longer in config.json "
                         "\"sources\" — a source that is no longer refreshed only "
                         "decays, and it still competes for slots in every search")
    args = ap.parse_args()
    if not (args.apply or args.dry_run):
        ap.error("pass --dry-run or --apply")

    db = store.connect()
    if args.purge_disabled:
        import config
        import ingest

        keep = set(config.load().get("sources", []))
        # "Disabled" means a connector that is no longer run, so its rows go
        # stale. A source with no connector at all is not disabled: `manual`
        # holds records written by hand, which nothing will ever regenerate.
        # Deleting those would be silent data loss.
        handwritten = {"manual", "intake"}
        connectors = set(getattr(ingest, "CONNECTORS", {}))
        present = {r["source"] for r in db.execute("SELECT DISTINCT source FROM docs")}
        drop = sorted(((present - keep) & connectors) - handwritten)
        skipped = sorted(present - keep - set(drop))
        if skipped:
            print(f"  keeping {skipped}: not connector-managed, nothing would "
                  f"regenerate these")
        for src in drop:
            n = db.execute("SELECT COUNT(*) FROM docs WHERE source=?", (src,)).fetchone()[0]
            print(f"  purging disabled source {src!r}: {n} docs")
            if args.apply:
                db.execute("DELETE FROM docs WHERE source=?", (src,))
        if args.apply:
            db.commit()
        if not drop:
            print("  no disabled sources present")
    total, dup_rows, spine, thin = report(db)
    print(f"before: {total} docs")
    print(f"  exact-duplicate rows (same source+content) : {dup_rows}")
    print(f"  undistilled raw-spine transcript fallbacks : {spine}")
    print(f"  near-empty docs (<80 chars of content)     : {thin}")
    if args.dry_run:
        keep = total - dup_rows - spine - (0 if args.keep_thin else thin)
        print(f"\nwould delete ~{total-keep} rows, leaving ~{keep}")
        return

    # 1. exact duplicates: keep the lowest id per (source, content)
    db.execute("""DELETE FROM docs WHERE id NOT IN (
                    SELECT MIN(id) FROM docs GROUP BY source, content)""")
    # 2. failed distills stored as raw transcript text
    db.execute("DELETE FROM docs WHERE source='transcripts' AND title LIKE 'Session in %'")
    # 3. contentless rows that can only ever be noise
    if not args.keep_thin:
        db.execute("DELETE FROM docs WHERE LENGTH(TRIM(COALESCE(content,'')))<80")
    db.commit()
    db.execute("INSERT INTO docs_fts(docs_fts) VALUES('rebuild')")
    db.commit()
    db.execute("VACUUM")

    total2, dup2, spine2, thin2 = report(db)
    print(f"\nafter:  {total2} docs  (-{total-total2})")
    print(f"  duplicates {dup2} | spine {spine2} | thin {thin2}")
    for r in store.stats(db):
        print(f"  {r['source']:12s} {r['n']:5d} docs  {r['embedded']} embedded")


if __name__ == "__main__":
    main()
