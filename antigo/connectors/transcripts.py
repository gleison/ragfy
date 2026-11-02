"""Session-transcript distiller — the only ingestion source that earns its keep.

Yesterday's Claude Code sessions in, TYPED ATOMIC RECORDS out. This is what
makes the knowledge base self-growing: you work normally, and what you learned
doing it becomes searchable tomorrow without you writing anything down.

One session does not become one summary. It becomes as many small records as it
actually contains, each embedded and retrieved independently:

  problem     something broke, and what fixed it. Written problem-first so a
              future prompt describing the SYMPTOM matches it, which is the
              whole point — hit the same wall twice, get the answer the second
              time without asking.
  fact        state of the world about Mason: what he is working on, plans,
              constraints, deadlines. Claude-memory-shaped, but searchable and
              not dependent on the memory mechanism.
  preference  how Mason wants work done, and why. Corrections and standing
              rules learned the hard way.
  decision    a choice made, what was rejected, and the reason.

Why atomic beats blob: a summary of a six-hour session is a blurry average of
six unrelated things. Its embedding sits near everything and close to nothing,
so it matches weak-but-constantly — the exact failure mode that filled the
context window with noise. One record per idea gives each a sharp vector.

The schema is FORCED. A record missing its required fields is dropped, never
softened into prose, because a malformed record still embeds and still competes
for a slot in every future search.
"""
import hashlib
import json
import sys
import threading
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timedelta, timezone
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent.parent))
import compress as _compress
import config
import llm
import store

PROJECTS = Path.home() / ".claude" / "projects"

# The distiller calls `claude -p`, and that call writes its OWN transcript into
# ~/.claude/projects. On the next run those look like fresh sessions, so the
# distiller distils its own output: the prompt it sent contained the original
# transcript, so the knowledge gets re-extracted second-hand, producing
# near-duplicate records and burning tokens on work already done. Left alone it
# compounds, because each pass creates more sessions than it consumes.
#
# Detected by signature rather than by directory, because the working directory
# of a distill worker is wherever the KB lives, which is also somewhere real
# work happens.
SELF_MARKERS = (
    "Extract durable knowledge from this Claude Code session transcript",
    "Distill this Claude Code session transcript",
    "Transcript (user/assistant spine, tool noise removed)",
)


def _is_self_generated(path, probe_bytes=16000):
    """True when this transcript is the distiller talking to itself."""
    try:
        with open(path, encoding="utf-8", errors="replace") as fh:
            head = fh.read(probe_bytes)
    except Exception:
        return False
    return any(m in head for m in SELF_MARKERS)
DISTILL_MODEL = None   # llm.py default (gpt-4o-mini)

PROMPT = """Extract durable knowledge from this Claude Code session transcript.

You are building a searchable long-term memory for this person's work. It will
be queried MONTHS from now by a different assistant that has no other context.
Extract only what would still be worth knowing then.

Return a JSON object with exactly these four keys, each an array (use [] when
the session has none — most sessions have none of at least one type):

"problems": something went wrong and was diagnosed or fixed.
  {{"problem": "the symptom as it was first observed, in the words someone
                hitting it again would use",
    "context": "where it happens — project, tool, file, command",
    "solution": "what actually fixed it, concretely enough to repeat",
    "root_cause": "why it happened ('' if never established)"}}

"facts": durable state about Mason, his projects, or his plans.
  {{"fact": "one self-contained statement, no pronouns, no 'today'/'currently'",
    "scope": "which area of his work this belongs to",
    "status": "current" | "planned" | "done"}}

"preferences": how Mason wants work done — corrections, standing rules, taste.
  {{"preference": "the rule, stated as an instruction",
    "why": "the reason he gave ('' if he didn't)",
    "applies_to": "when this rule is in force"}}

"decisions": a choice that was made and settled.
  {{"decision": "what was chosen",
    "rejected": "what was chosen over ('' if nothing specific)",
    "why": "the reason"}}

Rules:
- Extract nothing that is only true inside this session. "Fixed the typo on
  line 40" is not knowledge. "This pipeline crashes over 4GB of heap, run it
  with --max-old-space-size=12288" is.
- Extract nothing that a number or a date will falsify within weeks. Counts
  that move (subscribers, followers, revenue, row counts, task status), "as of
  today" snapshots, and in-progress percentages are NOT durable facts. A fact
  earns its place only if it would still be true and useful in six months.
- "problems" is the highest-value array. If the session diagnosed or fixed
  anything, capture it there even if the fix seems obvious in hindsight —
  hitting the same wall twice is exactly what this is for.
- Never extract anything identifying a specific patient, client, or customer.
  Extract the protocol, not the person it was applied to.
- No record about the process of the session itself (what was read, which agent
  ran, how long it took).
- Every field must be self-contained. It will be read with no surrounding text.
- Prefer 0 records over a vague one. An empty array is a correct answer.
- At most 4 per array. Fewer, sharper records beat more, vaguer ones.

Project directory: {project}

Transcript (user/assistant spine, tool noise removed):
{spine}

Reply with ONLY the JSON object."""

# kind -> (required fields, how the record renders for embedding + display)
SPEC = {
    "problem": (
        ("problem", "solution"),
        lambda r: (
            r["problem"][:110],
            "PROBLEM: {problem}\nWHERE: {context}\nSOLUTION: {solution}\nROOT CAUSE: {root_cause}".format(
                problem=r["problem"], context=r.get("context", ""),
                solution=r["solution"], root_cause=r.get("root_cause", "") or "not established"),
        ),
    ),
    "fact": (
        ("fact",),
        lambda r: (
            r["fact"][:110],
            "{fact}\nAREA: {scope}\nSTATUS: {status}".format(
                fact=r["fact"], scope=r.get("scope", ""), status=r.get("status", "current")),
        ),
    ),
    "preference": (
        ("preference",),
        lambda r: (
            r["preference"][:110],
            "PREFERENCE: {preference}\nWHY: {why}\nAPPLIES TO: {applies_to}".format(
                preference=r["preference"], why=r.get("why", "") or "not stated",
                applies_to=r.get("applies_to", "") or "generally"),
        ),
    ),
    "decision": (
        ("decision",),
        lambda r: (
            r["decision"][:110],
            "DECISION: {decision}\nWHY: {why}\nCHOSEN OVER: {rejected}".format(
                decision=r["decision"], why=r.get("why", "") or "not stated",
                rejected=r.get("rejected", "") or "no specific alternative"),
        ),
    ),
}
PLURAL = {"problem": "problems", "fact": "facts",
          "preference": "preferences", "decision": "decisions"}

MIN_FIELD_CHARS = 12
MAX_PER_KIND = 4
RETRIES = 2

STRICTER = ("\n\nYour previous reply was not valid JSON. Reply with the JSON "
            "object and NOTHING else — no prose before it, no explanation "
            "after it, no markdown fence.")


def _extract_json(text):
    """Pull the JSON object out of an LLM reply.

    A bare json.loads() failed on 2 of every 3 real sessions: the model fences
    it, prefaces it, or appends a sentence afterwards. Slicing to the outermost
    balanced braces recovers all of those without being fooled by braces inside
    string values."""
    t = text.strip()
    if t.startswith("```"):
        t = t.split("```", 2)[1] if t.count("```") >= 2 else t.strip("`")
        t = t.removeprefix("json").strip()
    start = t.find("{")
    if start < 0:
        raise ValueError("no JSON object in reply")
    depth, in_str, esc = 0, False, False
    for i, ch in enumerate(t[start:], start):
        if in_str:
            if esc:
                esc = False
            elif ch == "\\":
                esc = True
            elif ch == '"':
                in_str = False
            continue
        if ch == '"':
            in_str = True
        elif ch == "{":
            depth += 1
        elif ch == "}":
            depth -= 1
            if depth == 0:
                obj = json.loads(t[start:i + 1])
                if not isinstance(obj, dict):
                    raise ValueError("top level is not an object")
                return obj
    raise ValueError("unbalanced JSON object")


def _spine(path, cap=50000, shrink=True, skip_sidechain=False):
    """Compression happens BEFORE the cap, not after.

    Truncating first and compressing second wastes the budget: the model sees
    50k of raw text with acknowledgements and repeated status in it, and the
    tail of a long session is dropped to make room for filler in the middle.
    Squeezing first means the same 50k carries roughly twice the actual content,
    and most sessions stop needing to be cut at all."""
    lines = []
    with open(path, encoding="utf-8", errors="replace") as fh:
        for raw in fh:
            try:
                d = json.loads(raw)
            except ValueError:
                continue
            # Subagent turns would appear here as sidechain entries. They are
            # INCLUDED by default: work delegated to a subagent is still work
            # done in that session, and its findings are worth keeping. The
            # switch exists because that is a preference rather than a fact.
            # Measured on live transcripts: sidechain content is 0% of them, so
            # subagent runs are stored outside ~/.claude/projects and this path
            # currently matches nothing either way.
            if skip_sidechain and d.get("isSidechain"):
                continue
            content = (d.get("message") or {}).get("content")
            if d.get("type") == "user":
                if isinstance(content, str) and content.strip():
                    lines.append("USER: " + content.strip())
                elif isinstance(content, list):
                    lines += ["USER: " + b["text"].strip() for b in content
                              if isinstance(b, dict) and b.get("type") == "text" and b.get("text", "").strip()]
            elif d.get("type") == "assistant" and isinstance(content, list):
                lines += ["CLAUDE: " + b["text"].strip() for b in content
                          if isinstance(b, dict) and b.get("type") == "text" and b.get("text", "").strip()]
    text = "\n\n".join(lines)
    if shrink:
        text, stats = _compress.compress(text)
    if len(text) > cap:
        text = text[: cap // 2] + "\n\n[... middle trimmed ...]\n\n" + text[-(cap // 2):]
    return text


def _validate(kind, rec):
    """Enforce the schema. A record that fails is dropped, not repaired —
    a half-formed record embeds just as loudly as a good one."""
    required, _render = SPEC[kind]
    if not isinstance(rec, dict):
        return False
    for f in required:
        v = rec.get(f)
        if not isinstance(v, str) or len(v.strip()) < MIN_FIELD_CHARS:
            return False
    if kind == "fact" and rec.get("status") not in (None, "", "current", "planned", "done"):
        rec["status"] = "current"
    return True


def _records(art, project):
    """Typed JSON object -> flat list of (kind, title, content)."""
    out = []
    for kind, key in PLURAL.items():
        items = art.get(key) or []
        if not isinstance(items, list):
            continue
        for rec in items[:MAX_PER_KIND]:
            if not _validate(kind, rec):
                continue
            title, content = SPEC[kind][1](rec)
            out.append((kind, " ".join(title.split()), content))
    return out


def fetch(per_run=None, window_hours=None):
    cfg = config.load().get("transcripts", {})
    cap = per_run or cfg.get("per_run", 60)
    min_kb = cfg.get("min_kb", 50)
    settle_h = cfg.get("settle_hours", 6)
    import os
    # --backfill drains history; the daily cron path stays inside the window.
    window_h = 10 ** 9 if os.environ.get("KB_BACKFILL") else (
        window_hours or cfg.get("window_hours", 30))
    workers = cfg.get("workers", 4)

    db = store.connect()
    have = {r["source_id"] for r in db.execute(
        "SELECT source_id FROM docs WHERE source='transcripts'")}
    # Resuming a session writes a NEW .jsonl carrying the whole prior
    # conversation under a fresh sessionId (verified: no lineage field links
    # them), so the same work kept arriving as new and being re-distilled —
    # that produced 3.5k duplicate rows, 36% of the corpus. Hashing the
    # rendered record catches it regardless of which file it came from.
    have_hashes = {
        hashlib.sha1((r["content"] or "").encode()).hexdigest()
        for r in db.execute("SELECT content FROM docs WHERE source='transcripts'")
    }
    db.close()
    lock = threading.Lock()

    now_dt = datetime.now(timezone.utc)
    now = now_dt.timestamp()
    horizon = now - window_h * 3600
    cand, stale, selfmade = [], 0, 0
    for f in PROJECTS.glob("*/*.jsonl"):
        if f.name.startswith("agent-"):
            continue
        sid = f"{f.parent.name}:{f.stem}"
        st = f.stat()
        if sid in have or st.st_size < min_kb * 1024:
            continue
        if now - st.st_mtime < settle_h * 3600:
            continue            # still warm — the session may not be over
        if st.st_mtime < horizon:
            stale += 1
            continue            # outside the daily window; --backfill takes these
        if _is_self_generated(f):
            selfmade += 1
            continue
        cand.append((st.st_mtime, f, sid))
    cand.sort(reverse=True)
    todo = cand[:cap]
    print(f"  transcripts: {len(todo)} session(s) in the last {window_h}h "
          f"({stale} older skipped, {selfmade} of the distiller's own runs ignored)")

    def one(item):
        mtime, f, sid = item
        spine = _spine(f)
        if len(spine) < 500:
            return []
        ts = datetime.fromtimestamp(mtime, tz=timezone.utc).isoformat()
        project = f.parent.name
        base = PROMPT.format(project=project, spine=spine)
        art, last = None, ""
        for attempt in range(RETRIES):
            prompt = base if attempt == 0 else base + STRICTER
            try:
                text = llm.complete(prompt, DISTILL_MODEL, 2000).strip()
            except Exception as e:
                print(f"  {sid}: LLM unavailable ({str(e)[:80]}), retrying next run")
                return []
            if not text:
                last = "empty reply"
                continue
            try:
                art = _extract_json(text)
                break
            except Exception as e:
                last = str(e)[:80]
        if art is None:
            # Never store an unparsed response as prose. A malformed extraction
            # used to be kept as a raw transcript head; 469 of those piled up
            # and the retrieval eval scored every one of them as pure noise.
            print(f"  {sid}: schema violation after {RETRIES} tries ({last}), skipped")
            return []
        docs = []
        for i, (kind, title, content) in enumerate(_records(art, project)):
            digest = hashlib.sha1(content.encode()).hexdigest()
            with lock:
                if digest in have_hashes:
                    continue
                have_hashes.add(digest)
            docs.append(dict(
                source="transcripts", source_id=f"{sid}:{kind}:{i}", ts=ts,
                title=title, content=content, kind=kind, area="work",
                metadata={"project": project, "file": str(f), "digest": digest},
            ))
        return docs

    with ThreadPoolExecutor(max_workers=workers) as ex:
        docs = [d for batch in ex.map(one, todo) for d in batch]
    by_kind = {}
    for d in docs:
        by_kind[d["kind"]] = by_kind.get(d["kind"], 0) + 1
    print(f"  transcripts: {len(docs)} records from {len(todo)} sessions  {by_kind or '{}'}")
    return docs
