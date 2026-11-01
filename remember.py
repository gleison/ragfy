#!/usr/bin/env python3
"""Write something into the knowledge base right now, not at the next ingest.

Ingestion runs on a schedule, which is right for bulk material but wrong for the
thing you just said. A decision made in this conversation, a fix you just found,
a preference you just stated: none of it is retrievable until the next run, and
by then the session that produced it is over. This closes that window. The
record is stored, embedded, and searchable within a second.

  python remember.py problem "Renders die over 4GB heap" \\
      --solution "run with --max-old-space-size=12288" --area work
  python remember.py fact "The lab turnaround is now 10 working days"
  python remember.py decision "Standardised on rubber dam for all posteriors" \\
      --why "cotton roll isolation caused remakes"
  python remember.py preference "Never schedule surgical cases after 3pm" \\
      --why "fatigue errors" --applies-to scheduling

Kinds match what the transcript distiller produces, so a record written by hand
and one extracted automatically are indistinguishable to retrieval.
"""
import argparse
import hashlib
import sys
from datetime import datetime, timezone
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import config  # noqa: F401
import store

KINDS = ("problem", "fact", "preference", "decision", "note")


def render(kind, text, args):
    """Same shape the distiller emits, so retrieval treats both identically."""
    if kind == "problem":
        return ("PROBLEM: {t}\nWHERE: {w}\nSOLUTION: {s}\nROOT CAUSE: {r}".format(
            t=text, w=args.where or "", s=args.solution or "not recorded",
            r=args.root_cause or "not established"))
    if kind == "decision":
        return ("DECISION: {t}\nWHY: {w}\nCHOSEN OVER: {o}".format(
            t=text, w=args.why or "not stated", o=args.rejected or "no specific alternative"))
    if kind == "preference":
        return ("PREFERENCE: {t}\nWHY: {w}\nAPPLIES TO: {a}".format(
            t=text, w=args.why or "not stated", a=args.applies_to or "generally"))
    if kind == "fact":
        return "{t}\nAREA: {a}\nSTATUS: {s}".format(
            t=text, a=args.area or "", s=args.status or "current")
    return text


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("kind", choices=KINDS)
    ap.add_argument("text")
    ap.add_argument("--area")
    ap.add_argument("--why")
    ap.add_argument("--where")
    ap.add_argument("--solution")
    ap.add_argument("--root-cause")
    ap.add_argument("--rejected")
    ap.add_argument("--applies-to")
    ap.add_argument("--status", choices=["current", "planned", "done"])
    ap.add_argument("--confidence", type=float, default=1.0,
                    help="0-1. Something you stated outright is 1.0; something "
                         "inferred should be lower, and ranks lower.")
    ap.add_argument("--quiet", action="store_true")
    args = ap.parse_args()

    if len(args.text.strip()) < 12:
        raise SystemExit("too short to be worth storing")

    content = render(args.kind, args.text.strip(), args)
    digest = hashlib.sha1(content.encode()).hexdigest()
    db = store.connect()
    dup = db.execute(
        "SELECT id, title FROM docs WHERE source='manual' AND source_id=?",
        (digest[:16],)).fetchone()
    if dup:
        print(f"already stored: {dup['title'][:70]}")
        return

    store.upsert(db, "manual", digest[:16], datetime.now(timezone.utc).isoformat(),
                 " ".join(args.text.strip().split())[:110], content,
                 {"confidence": max(0.0, min(1.0, args.confidence)), "by": "hand"},
                 args.kind, args.area)
    db.commit()
    store.embed_missing(db)          # searchable now, not after the next run
    if not args.quiet:
        print(f"stored [{args.kind}] and embedded: {args.text.strip()[:70]}")


if __name__ == "__main__":
    main()
