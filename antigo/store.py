"""NSM Knowledge Base — storage + embeddings + hybrid search.

One SQLite database, one docs table. Every connector emits rows with the
same shape (source, source_id, ts, title, content, metadata) and anything
in the table is immediately searchable through the same interface.
"""
import json
import math
import os
import sqlite3
import struct
from datetime import datetime, timezone
from pathlib import Path

import numpy as np

DB_PATH = Path(__file__).parent / "kb.sqlite"
_MATRIX = {}  # cached normalised embedding matrix, cleared on connect()
_last_sims = {}  # doc_id -> cosine from the most recent search(), for inspection
_QVEC = {}       # text -> normalised query vector; calibration re-asks the same
                 # questions across every threshold combination, and paying the
                 # embeddings API each time is both slow and pointless.
_QVEC_MAX = 2000


def _embed_queries(texts):
    missing = [t for t in texts if t not in _QVEC]
    if missing:
        mat = embed_texts(missing)
        if len(_QVEC) > _QVEC_MAX:
            _QVEC.clear()
        for t, v in zip(missing, mat):
            _QVEC[t] = v
    return np.stack([_QVEC[t] for t in texts])
EMBED_MODEL = "text-embedding-3-small"
EMBED_DIMS = 1536

# Embedding provider. "openai" is better quality; "local" never sends your text
# anywhere. Which one is correct depends on what is in your library, not on
# which is technically superior: for material under professional
# confidentiality, a slightly weaker model that runs on your own machine beats a
# stronger one you are not allowed to use.
#
# The two produce incompatible vectors (different models, different dimensions),
# so switching providers means re-embedding everything. `_provider_guard` below
# refuses to mix them rather than silently returning nonsense.
# bge-base-en-v1.5 scores 63.55 MTEB average against text-embedding-3-small's
# 62.3, so running locally is not the quality sacrifice people assume. Use
# bge-small-en-v1.5 (384 dims, 62.17) if disk or CPU is tight.
DEFAULT_LOCAL_MODEL = "BAAI/bge-base-en-v1.5"
_LOCAL = {}


def _embed_cfg():
    import config

    return config.load().get("embedding", {}) or {}


def _provider():
    cfg = _embed_cfg()
    prov = cfg.get("provider", "openai")
    model = cfg.get("model") or (DEFAULT_LOCAL_MODEL if prov == "local" else EMBED_MODEL)
    return prov, model


def _local_model(name):
    """Loaded once per process. First call downloads the weights (~130MB for the
    default) and is slow; afterwards it is fast and entirely offline."""
    if name not in _LOCAL:
        try:
            from sentence_transformers import SentenceTransformer
        except ImportError:
            raise RuntimeError(
                "local embeddings need sentence-transformers: "
                "pip install sentence-transformers")
        _LOCAL[name] = SentenceTransformer(name)
    return _LOCAL[name]


def embed_texts(texts):
    """Embed a list of strings. Returns a normalised float32 matrix.

    Any provider speaking the OpenAI embeddings API works by setting
    `base_url` and `api_key_env` in config, which covers Azure OpenAI, Voyage,
    and most hosted gateways without new code. That matters more than it looks:
    the confidentiality question is usually answered by WHICH account the same
    model runs under, not by switching to a weaker model."""
    prov, model = _provider()
    if prov not in ("local", "ollama") and local_only():
        raise RuntimeError(
            "privacy.local_only is set but embedding.provider is "
            f"'{prov}'. Set embedding.provider to 'local', or turn local_only off.")
    if prov == "local":
        vecs = _local_model(model).encode(
            [_embed_truncate(t, 400) for t in texts],
            normalize_embeddings=True, show_progress_bar=False)
        return np.asarray(vecs, dtype=np.float32)
    import os

    from openai import OpenAI

    cfg = _embed_cfg()
    kwargs = {}
    if cfg.get("base_url"):
        kwargs["base_url"] = cfg["base_url"]
    key_env = cfg.get("api_key_env", "OPENAI_API_KEY")
    if os.environ.get(key_env):
        kwargs["api_key"] = os.environ[key_env]
    resp = OpenAI(**kwargs).embeddings.create(
        model=model, input=[_embed_truncate(t) for t in texts])
    mat = np.stack([np.asarray(d.embedding, dtype=np.float32) for d in resp.data])
    return mat / (np.linalg.norm(mat, axis=1, keepdims=True) + 1e-9)


def _provider_guard(db, dims=None):
    """Refuse to mix vectors from different models.

    Checked BEFORE the first embedding call, not after: a provider mismatch is
    knowable from configuration alone, and discovering it after a batch has been
    sent means paying for vectors that are about to be rejected.

    Two providers produce vectors that are not comparable, and cosine similarity
    between them is meaningless rather than merely worse. Without this check a
    provider switch half-fills the table with incompatible vectors and retrieval
    quietly returns noise, which is very hard to diagnose after the fact."""
    prov, model = _provider()
    db.execute("CREATE TABLE IF NOT EXISTS kb_meta (key TEXT PRIMARY KEY, value TEXT)")
    row = db.execute("SELECT value FROM kb_meta WHERE key='embedding'").fetchone()
    if row is None:
        if dims is not None:
            db.execute("INSERT INTO kb_meta (key, value) VALUES ('embedding', ?)",
                       (f"{prov}:{model}:{dims}",))
            db.commit()
        return
    stored_prov, stored_model, stored_dims = (row["value"].rsplit(":", 2) + ["", ""])[:3]
    tag = f"{prov}:{model}:{dims if dims is not None else stored_dims}"
    if (stored_prov, stored_model) != (prov, model) or row["value"] != tag:
        raise RuntimeError(
            f"This library was built with {row['value']} but the configuration now "
            f"says {tag}. Vectors from different models cannot be compared. Either "
            f"restore the old setting, or re-embed everything:\n"
            f"  sqlite3 kb.sqlite \"UPDATE docs SET embedding=NULL; "
            f"DELETE FROM kb_meta WHERE key='embedding';\"\n"
            f"  python ingest.py")

SCHEMA = """
CREATE TABLE IF NOT EXISTS docs (
    id INTEGER PRIMARY KEY,
    source TEXT NOT NULL,
    source_id TEXT NOT NULL,
    ts TEXT,
    title TEXT,
    content TEXT,
    metadata TEXT,
    embedding BLOB,
    kind TEXT,
    area TEXT,
    UNIQUE(source, source_id)
);
CREATE VIRTUAL TABLE IF NOT EXISTS docs_fts USING fts5(
    title, content, content='docs', content_rowid='id'
);
CREATE TRIGGER IF NOT EXISTS docs_ai AFTER INSERT ON docs BEGIN
    INSERT INTO docs_fts(rowid, title, content) VALUES (new.id, new.title, new.content);
END;
CREATE TRIGGER IF NOT EXISTS docs_ad AFTER DELETE ON docs BEGIN
    INSERT INTO docs_fts(docs_fts, rowid, title, content) VALUES ('delete', old.id, old.title, old.content);
END;
CREATE TRIGGER IF NOT EXISTS docs_au AFTER UPDATE ON docs BEGIN
    INSERT INTO docs_fts(docs_fts, rowid, title, content) VALUES ('delete', old.id, old.title, old.content);
    INSERT INTO docs_fts(rowid, title, content) VALUES (new.id, new.title, new.content);
END;
"""


def connect():
    _MATRIX.clear()  # a new connection may see a changed corpus
    db = sqlite3.connect(DB_PATH)
    db.executescript(SCHEMA)
    # `kind` was added after the table existed; ALTER is a no-op on new DBs.
    cols = {r[1] for r in db.execute("PRAGMA table_info(docs)")}
    for col in ("kind TEXT", "area TEXT"):
        if col.split()[0] not in cols:
            db.execute(f"ALTER TABLE docs ADD COLUMN {col}")
    db.row_factory = sqlite3.Row
    return db


def upsert(db, source, source_id, ts, title, content, metadata=None, kind=None, area=None):
    """Insert or replace a doc. Clears the embedding when content changes."""
    row = db.execute(
        "SELECT id, content FROM docs WHERE source=? AND source_id=?",
        (source, source_id),
    ).fetchone()
    meta = json.dumps(metadata or {})
    if row is None:
        db.execute(
            "INSERT INTO docs (source, source_id, ts, title, content, metadata, kind, area) "
            "VALUES (?,?,?,?,?,?,?,?)",
            (source, source_id, ts, title, content, meta, kind, area),
        )
        return True
    if row["content"] != content:
        db.execute(
            "UPDATE docs SET ts=?, title=?, content=?, metadata=?, kind=?, area=?, "
            "embedding=NULL WHERE id=?",
            (ts, title, content, meta, kind, area, row["id"]),
        )
        return True
    return False


def _embed_truncate(text, max_tokens=8000):
    """Hard-cap text at the embedding model's 8192-token input limit.
    Char caps aren't enough — dense content (CSV, code) can exceed the
    token limit well under 24k chars."""
    try:
        import tiktoken

        enc = tiktoken.get_encoding("cl100k_base")
        toks = enc.encode(text[:100000], disallowed_special=())
        return enc.decode(toks[:max_tokens]) if len(toks) > max_tokens else text[:100000]
    except Exception:
        return text[:8000]  # no tiktoken: 8k chars can't exceed 8k tokens


def embed_missing(db, batch_size=100):
    """Embed every doc that doesn't have a vector yet."""
    rows = db.execute("SELECT id, title, content FROM docs WHERE embedding IS NULL").fetchall()
    if not rows:
        return 0
    _provider_guard(db)          # fail before spending anything
    checked = False
    for i in range(0, len(rows), batch_size):
        batch = rows[i : i + batch_size]
        texts = [(r["title"] or "") + "\n" + (r["content"] or "") for r in batch]
        mat = embed_texts(texts)
        if not checked:
            _provider_guard(db, mat.shape[1])
            checked = True
        for r, vec in zip(batch, mat):
            db.execute("UPDATE docs SET embedding=? WHERE id=?",
                       (vec.astype(np.float32).tobytes(), r["id"]))
        db.commit()
        print(f"  embedded {min(i + batch_size, len(rows))}/{len(rows)}")
    return len(rows)


# Stopwords are why the lexical arm used to match 95% of the corpus: a prompt
# like "I want to make some changes to the hook" OR-joined every word, so any
# doc containing "want" or "make" became a BM25 candidate and rode RRF into the
# context window. Only content-bearing terms are allowed to match.
STOPWORDS = frozenset("""
a about actually add all also am an and any anything are as at back be because been
before being both but by can cant could did didnt do does doesnt doing done dont down
each even every for from get gets getting give go going good got had has have having
he her here hers him his how i id if ill im in into is isnt it its ive just keep know
let like ll look looking made make makes making many maybe me might mine more most much
must my need needs new no not now of off ok okay on once one only or other ought our
out over own please put re really right same say see seen should show so some something
still such sure take tell than thank thanks that thats the their them then there these
they thing things think this those though through time to too try trying up us use used
using very want wants was way we well went were what when where which while who why will
with wont would yeah yes yet you your youre
""".split())

MIN_TERM_LEN = 3


def _terms(query):
    """Content-bearing terms only: stopwords out, punctuation stripped, short
    tokens dropped unless they look like an identifier (LUT, API, KB)."""
    out = []
    for raw in query.split():
        t = "".join(ch for ch in raw if ch.isalnum() or ch in "-_./").strip("-_./")
        if not t:
            continue
        low = t.lower()
        if low in STOPWORDS:
            continue
        if len(t) < MIN_TERM_LEN and not t.isupper():
            continue
        if low not in out:
            out.append(low)
    return out


def _fts_query(query, stopwords=True):
    """Returns an FTS5 MATCH expression, or None when the query carries no
    searchable content (chit-chat, acknowledgements) — the caller then skips
    the lexical arm entirely rather than matching on filler."""
    terms = _terms(query) if stopwords else [
        t.replace('"', "") for t in query.split() if t.strip('"')
    ]
    if not terms:
        return None
    return " OR ".join(f'"{t}"' for t in terms)


def _recency_cfg():
    import config

    return config.load().get("recency", {})


def _age_factor(ts, now, kind=None, cfg=None):
    """Relevance decay by age, per record type.

    Mason's spec was "drops off super fast, then slower and slower, so we keep a
    really long tail". That is a POWER LAW, not an exponential — exponential
    decays at a constant relative rate, so it has no tail at all. The old
    setting (exponential, 90-day) multiplied a one-year-old doc by 0.017, which
    is deletion in all but name.

        power law  1 / (1 + age/tau)^alpha
        age  30d    90d    1y     2y
        exp  0.72   0.37   0.017  0.0003
        pow  0.50   0.25   0.076  0.039     (tau=30, alpha=1)

    The curve is per KIND because staleness is not one phenomenon. A fact
    ("the pipeline currently renders at 1080p") rots in weeks. A problem and
    its solution does not rot at all — a fix from last year is still the fix,
    and finding it is the entire point of the system. Preferences are the most
    durable of all: a standing rule Mason gave once should still surface a year
    later. Tunable in config.json {"recency": {...}}.
    """
    cfg = _recency_cfg() if cfg is None else cfg
    curve = (cfg.get("by_kind", {}) or {}).get(kind or "", None) or cfg.get(
        "default", {"tau_days": 60, "alpha": 0.9})
    tau = float(curve.get("tau_days", 60)) or 60.0
    alpha = float(curve.get("alpha", 0.9))
    floor = float(cfg.get("floor", 0.0))
    if not ts:
        return max(floor, 0.5)  # undated: treat as middle-aged, not fresh
    try:
        dt = datetime.fromisoformat(ts.replace("Z", "+00:00"))
        if dt.tzinfo is None:
            dt = dt.replace(tzinfo=timezone.utc)
        age_days = max(0.0, (now - dt).total_seconds() / 86400)
    except ValueError:
        return max(floor, 0.5)
    if cfg.get("model") == "exponential":  # kept so the old curve is reachable
        return max(floor, math.exp(-age_days / tau))
    return max(floor, 1.0 / (1.0 + age_days / tau) ** alpha)


def usage_index(db, cfg=None):
    """doc_id -> (qualifying_uses, days_since_last_qualifying_use).

    Only STRONG retrievals count. A retrieval qualifies when the document was a
    close match (`sim` above the strong threshold) and near the top of the list
    (`rank` inside the cap). Bare retrieval count would be a feedback loop: the
    system boosts what it served, then serves it more because it is boosted, so
    a weak record that once slipped in as filler ratchets its way to the top.
    Requiring a strong match to earn credit breaks that loop, because a record
    that only ever appears as marginal filler never accrues any.

    A retrieval the curator's audit marked useless (verdict 0) is excluded
    outright, and one it confirmed (verdict 1) always counts."""
    cfg = cfg or {}
    strong = float(cfg.get("strong_similarity", 0.50))
    rank_cap = int(cfg.get("credit_rank_cap", 3))
    try:
        rows = db.execute(
            """SELECT doc_id, COUNT(*) AS n, MAX(ts) AS last FROM retrievals
               WHERE COALESCE(verdict, 1) <> 0
                 AND (verdict = 1
                      OR (COALESCE(sim, 0) >= ? AND COALESCE(rank, 99) <= ?))
               GROUP BY doc_id""",
            (strong, rank_cap),
        ).fetchall()
    except Exception:
        return {}
    now = datetime.now(timezone.utc)
    out = {}
    for r in rows:
        try:
            last = datetime.fromisoformat((r["last"] or "").replace("Z", "+00:00"))
            if last.tzinfo is None:
                last = last.replace(tzinfo=timezone.utc)
            days = max(0.0, (now - last).total_seconds() / 86400)
        except ValueError:
            days = 9e9
        out[r["doc_id"]] = (int(r["n"]), days)
    return out


def _usage_boost(entry, cfg):
    """Multiplier from how much a document has actually been used.

    Age tells you how old a record is, which is only a proxy. Use tells you
    whether it was ever worth anything. A three-year-old record retrieved every
    day is the most valuable thing in the corpus; a three-week-old one that has
    never fired is dead weight. Frequency and recency-of-USE both count, so a
    record that mattered during one project fades once the project ends, without
    anyone having to decide that.

    Never-used docs return 1.0 rather than a penalty — a new record has no
    history yet, and starving it would stop it ever earning one."""
    if not cfg.get("enabled", True) or not entry:
        return 1.0
    n, days_since = entry
    cw = float(cfg.get("count_weight", 0.45))
    rw = float(cfg.get("recent_weight", 0.55))
    tau = float(cfg.get("recent_tau_days", 21)) or 21.0
    freq = math.log1p(n)                      # 1 use -> .69, 10 -> 2.4, 100 -> 4.6
    recent = math.exp(-days_since / tau)      # used today -> 1, a month ago -> .24
    boost = 1.0 + cw * freq + rw * recent
    return min(boost, float(cfg.get("max_boost", 3.0)))


RETRIEVALS_SCHEMA = """
CREATE TABLE IF NOT EXISTS retrievals (
    doc_id INTEGER NOT NULL,
    ts TEXT NOT NULL,
    sim REAL,
    rank INTEGER,
    verdict INTEGER,
    query TEXT,
    queries TEXT,
    session TEXT,
    source_agent TEXT,
    judged_by TEXT
);
CREATE INDEX IF NOT EXISTS retrievals_doc ON retrievals(doc_id);
"""


def log_retrievals(db, events):
    """Record HOW a doc was served, not merely that it was.

    Counting bare retrievals is circular: retrieval quality is the thing being
    measured, so a junk record pulled as filler earns credit for being pulled,
    gets boosted, and gets pulled more. `sim` and `rank` are stored so usage
    credit can be restricted to retrievals that were actually strong matches,
    and `verdict` is left null for the curator's audit to fill in."""
    try:
        db.executescript(RETRIEVALS_SCHEMA)
        for col in ("sim REAL", "rank INTEGER", "verdict INTEGER", "query TEXT",
                    "queries TEXT", "session TEXT", "source_agent TEXT",
                    "judged_by TEXT"):
            name = col.split()[0]
            if name not in {r[1] for r in db.execute("PRAGMA table_info(retrievals)")}:
                db.execute(f"ALTER TABLE retrievals ADD COLUMN {col}")
        now = datetime.now(timezone.utc).isoformat()
        db.executemany(
            "INSERT INTO retrievals (doc_id, ts, sim, rank, verdict, query, queries, "
            "session, source_agent) VALUES (?,?,?,?,NULL,?,?,?,?)",
            [(e["id"], now, e.get("sim"), e.get("rank"), e.get("query"),
              e.get("queries"), e.get("session"), e.get("agent")) for e in events])
        db.commit()
    except Exception:
        pass  # never let bookkeeping break a prompt


def _retrieval_cfg():
    import config

    return config.load().get("retrieval", {})


def _vectors(db, source=None):
    """All embeddings as one normalised matrix. Cached per connection+source so
    a batch of queries pays the load once. This is a brute-force O(N) scan —
    correct and fast to ~100k docs; past that swap in an ANN index (see
    VECTOR_BACKENDS.md)."""
    key = (id(db), source)
    if key in _MATRIX:
        return _MATRIX[key]
    where = " WHERE embedding IS NOT NULL" + (" AND source=?" if source else "")
    rows = db.execute(f"SELECT id, embedding FROM docs{where}", [source] if source else []).fetchall()
    if not rows:
        return np.zeros((0, EMBED_DIMS), dtype=np.float32), []
    mat = np.stack([np.frombuffer(r["embedding"], dtype=np.float32) for r in rows])
    mat /= np.linalg.norm(mat, axis=1, keepdims=True) + 1e-9
    ids = [r["id"] for r in rows]
    _MATRIX[key] = (mat, ids)
    return mat, ids


def _norm_title(t):
    return " ".join((t or "").lower().split())[:120]


def _segments(query, max_segments=8, min_terms=3):
    """Split a prompt into separately-searchable thoughts.

    Embedding a whole message as one vector averages every idea in it. A prompt
    that asks about two things produces a vector sitting between them and near
    neither, so it matches documents that share scattered vocabulary with the
    blend rather than documents that answer either question. Mason's prompts are
    spoken and long, which makes this the dominant source of irrelevant hits.

    Scoring each thought separately and taking the best match per document means
    a record only has to answer ONE thing well, instead of resembling an average
    of everything asked."""
    import re

    parts = [p.strip() for p in re.split(r"(?<=[.!?;])\s+|\n+", query) if p.strip()]
    segs = [p for p in parts if len(_terms(p)) >= min_terms]
    # Merge runts into their neighbour so a short clause is not lost entirely.
    if not segs:
        segs = [query]
    if len(segs) > max_segments:
        segs = sorted(segs, key=lambda x: -len(_terms(x)))[:max_segments]
    # The full prompt stays in the set: some questions only make sense whole.
    if query.strip() not in segs:
        segs.append(query.strip())
    return segs[: max_segments + 1]


DISTILL_PROMPT = """Today is {today}.

Turn this message into the 1-3 search queries that should be run against Mason's personal
knowledge base. That base holds records of the form: problems he hit and how they were fixed,
standing preferences and rules he has given, decisions he made and why, durable facts about his
projects, and dated calendar events.

Rules:

1. If the message ASKS YOU TO DO SOMETHING, do not search for the instruction. Search for the
   prior experience needed to do it well: the rules that govern that kind of work, how it was
   done before, and problems previously hit doing it. Write those as statements, not commands.
   "Cut and grade the take I just recorded"
     -> ["video grading LUT and vignette settings", "video cutting pipeline and silence
          detection settings", "problems encountered cutting and grading video"]

2. If the message refers to a time RELATIVE to today, resolve it to the actual weekday and date,
   and include both. Calendar records are written as full sentences containing the weekday, the
   month name, the numeric date and a 12-hour time.
   "What's on my calendar this coming Monday" (today being Friday 2026-08-21)
     -> ["calendar events on Monday, August 24, 2026", "meetings scheduled Monday August 24"]

3. If it asks a direct question, write the query as the ANSWER would be phrased, not as the
   question. "When is my next exam" -> ["exam scheduled", "exam date and class"].

4. Keep proper nouns, filenames, error text and numbers exactly as written. They are the
   strongest match signal there is.

5. Return NO queries only when the message contains no information need at all: greetings,
   thanks, acknowledgement, approval ("yes do that"), or an instruction purely about this
   conversation ("stop", "start over", "explain what you just said").

   Do NOT return an empty list because you doubt the library contains the answer. That is not
   your decision and you cannot see the library. Any message asking about a topic gets queries,
   including general-knowledge questions. Whether anything relevant exists is decided afterwards
   by measuring similarity against the actual contents, which is evidence rather than a guess.
   Guessing here silently drops real questions.

Reply with ONLY a JSON object:
  {{"queries": [...], "dates": ["YYYY-MM-DD"], "tense": "future"|"past"|null, "kinds": [...]}}

"dates" lists every specific calendar date the message asks about, resolved against today.
A range means listing each day in it, up to 14. Use [] when no particular date is named.
"tomorrow" is one date; "next week" is the seven dates of that week.

"kinds" names the record types most likely to hold the answer, from: problem (something
broke and was fixed), fact (durable state), preference (a standing rule), decision (a settled
choice), event (a dated calendar entry), document (reference material). Use [] when unsure.
"What did I decide about X" -> ["decision"]. "X keeps failing" -> ["problem"]. "How do I like
Y done" -> ["preference"]. "When is Z" -> ["event"]. This only nudges the ranking, so a wrong
guess costs little and an empty list costs nothing.

"tense" is "future" when the message asks about something upcoming ("my next exam", "what's
coming up", "when do I meet"), "past" when it asks about something already happened ("what did
I do", "when did I last"), and null otherwise. Set it even when "dates" is empty: it is how
"when is my next exam" avoids being answered with last semester's exam.

MESSAGE: {msg}"""


def local_only():
    """True when this library is configured to let nothing leave the machine."""
    import config

    return bool(config.load().get("privacy", {}).get("local_only"))


def _ollama_distill(prompt, cfg):
    """Query rewriting through a model running on this machine.

    Ollama listens on localhost and sends nothing outward, so this keeps the
    quality benefit of rewriting the query without the prompt leaving the
    machine. Requires ollama running with the configured model pulled."""
    import json as _json
    import urllib.request

    body = _json.dumps({
        "model": cfg.get("ollama_model", "llama3.2"),
        "prompt": prompt,
        "stream": False,
        "options": {"temperature": 0},
    }).encode()
    req = urllib.request.Request(
        cfg.get("ollama_url", "http://127.0.0.1:11434/api/generate"),
        data=body, headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=float(cfg.get("timeout_s", 10))) as r:
        return _json.loads(r.read().decode()).get("response", "")


def distill_query(query, cfg=None):
    """Turn a spoken, rambling prompt into the queries it is actually asking.

    Embedding a raw message searches for text that RESEMBLES the message, which
    is not the same as text that ANSWERS it. Filler, hedging and second thoughts
    all get a vote in the average. Measured against splitting on punctuation,
    distilling first raises the best match by 0.07-0.08 cosine on real prompts,
    and it returns an empty list for messages that ask nothing at all, which no
    amount of similarity thresholding does as cleanly.

    Returns None on any failure so the caller falls back to segmentation. This
    runs inside a prompt hook: it must never be the reason a prompt breaks."""
    cfg = cfg or {}
    if not cfg.get("enabled", True):
        return None
    provider = cfg.get("provider", "openai")
    # A library configured as local-only must not send the PROMPT out either.
    # Embedding locally while still shipping every question to a remote model
    # would leak the more sensitive half of the exchange.
    if local_only() and provider == "openai":
        return None
    prompt = DISTILL_PROMPT.format(
        msg=query[:4000], today=datetime.now().strftime("%A, %B %-d, %Y"))
    try:
        if provider == "ollama":
            out = _ollama_distill(prompt, cfg).strip()
        else:
            from openai import OpenAI

            out = OpenAI(timeout=float(cfg.get("timeout_s", 4))).chat.completions.create(
                model=cfg.get("model", "gpt-4o-mini"),
                messages=[{"role": "user", "content": prompt}],
                temperature=0, max_tokens=200,
            ).choices[0].message.content.strip()
        start, end = out.find("{"), out.rfind("}")
        if start < 0 or end < start:
            return None
        obj = json.loads(out[start:end + 1])
        if not isinstance(obj, dict):
            return None
        qs = [q.strip() for q in obj.get("queries") or [] if isinstance(q, str) and q.strip()]
        dates = [d for d in obj.get("dates") or []
                 if isinstance(d, str) and len(d) == 10 and d[4] == "-"]
        tense = obj.get("tense") if obj.get("tense") in ("future", "past") else None
        kinds = [k for k in obj.get("kinds") or []
                 if isinstance(k, str) and k in
                 ("problem", "fact", "preference", "decision", "event", "document",
                  "note", "daily-note")]
        return {"queries": qs[: int(cfg.get("max_queries", 3))],
                "dates": dates[:14], "tense": tense, "kinds": kinds[:3]}
    except Exception:
        return None


def search(db, query, limit=10, source=None, rrf_k=60,
           floor=None, max_per_source=None, stopwords=True, mmr=None,
           candidates=50, log=False, session=None, agent=None, area=None,
           max_per_parent=None, min_top=None, distill=True, queries=None):
    """Hybrid retrieval: FTS5 BM25 + vector cosine, fused with RRF + age decay,
    then gated and diversified.

    Three defences sit on top of the fusion, because RRF on its own is purely
    *relative* — it will always rank something first, even when the corpus holds
    nothing relevant at all:

      floor           absolute cosine a doc must clear to be eligible. Measured
                      on this corpus: unanswerable questions top out at 0.265,
                      genuinely answerable ones start at 0.432. Default 0.35
                      sits in that empty band, so an off-topic prompt now
                      retrieves NOTHING instead of eight confident non-answers.
      max_per_source  stops one source (transcripts is 78% of the corpus) from
                      taking every slot.
      mmr             near-duplicate suppression. Resumed Claude Code sessions
                      produce many near-identical distillations; without this
                      one piece of work fills the whole context block.

    Passing floor=0.0, max_per_source=99, mmr=0.0 restores the old behaviour
    (used by the eval harness to reproduce a baseline).
    """
    cfg = _retrieval_cfg()
    floor = cfg.get("min_similarity", 0.45) if floor is None else floor
    if min_top is not None:
        top_floor = min_top
    else:
        top_floor = cfg.get("min_top_similarity", 0.50) if floor else 0.0
    max_per_source = cfg.get("max_per_source", 4) if max_per_source is None else max_per_source
    mmr = cfg.get("dedupe_similarity", 0.93) if mmr is None else mmr
    max_per_parent = cfg.get("max_per_parent", 2) if max_per_parent is None else max_per_parent

    import config as _cfgmod
    if queries is not None:
        # Pre-distilled by the caller. Calibration sweeps the thresholds over the
        # same questions a hundred times; distilling once and reusing keeps the
        # comparison honest and stops it costing a hundred LLM calls per question.
        _d = queries if isinstance(queries, dict) else {"queries": queries}
        distilled = {"queries": list(_d.get("queries") or []),
                     "dates": _d.get("dates") or [],
                     "tense": _d.get("tense"),
                     "kinds": _d.get("kinds") or []}
    elif not distill:
        distilled = None
    else:
        distilled = distill_query(query, _cfgmod.load().get("query_distill", {}))
    dates = (distilled or {}).get("dates") or []
    tense = (distilled or {}).get("tense")
    want_kinds = set((distilled or {}).get("kinds") or [])
    if distilled is not None and not distilled["queries"] and not dates:
        return []          # the model found no information need in the message
    if distilled and distilled["queries"]:
        segs = distilled["queries"]
    elif cfg.get("segment_query", True):
        segs = _segments(query)   # fallback: split on punctuation
    else:
        segs = [query]

    # Dated events are matched by DATE, not by similarity. "Monday, August 24"
    # and "Labor Day" are near-identical to an embedding model — both read as a
    # holiday-shaped calendar entry — so asking what is on a given day used to
    # return whichever event sounded most calendar-like. A date is a fact, and
    # facts belong in a WHERE clause.
    today = datetime.now(timezone.utc).strftime("%Y-%m-%d")
    dated = []
    if dates:
        rows = db.execute(
            "SELECT * FROM docs WHERE kind='event' AND SUBSTR(ts,1,10) IN "
            f"({','.join('?' * len(dates))}) ORDER BY ts", dates).fetchall()
        dated = [(9.0 - i * 0.001, r) for i, r in enumerate(rows)]
    elif tense == "future":
        # "When is my next exam" must not be answered with last semester's exam.
        # Tense is a filter on time, and no similarity score encodes it.
        rows = db.execute(
            "SELECT * FROM docs WHERE kind='event' AND SUBSTR(ts,1,10) >= ? ORDER BY ts LIMIT 200",
            (today,)).fetchall()
        dated = []
        _future_ids = {r["id"] for r in rows}
    qmat = _embed_queries(segs)
    mat, ids = _vectors(db, source)
    if len(ids) == 0:
        return []
    if area:
        keep = {r["id"] for r in db.execute(
            "SELECT id FROM docs WHERE area = ?", (area,))}
        sel = [i for i, d in enumerate(ids) if d in keep]
        if not sel:
            return []
        mat, ids = mat[sel], [ids[i] for i in sel]
    # Best-matching thought wins, rather than the average of all of them.
    sims = (mat @ qmat.T).max(axis=1)
    # Query gate. `floor` decides which docs are good enough; this decides
    # whether the question is answerable from this corpus AT ALL. Chit-chat
    # ("thanks, keep going") and general coding questions have no best match
    # worth surfacing — without this they still scrape together a few docs that
    # clear the per-doc floor by accident.
    if top_floor and (len(sims) == 0 or float(sims.max()) < top_floor):
        return []
    cos = {doc_id: float(s) for doc_id, s in zip(ids, sims)}
    _last_sims.clear()
    _last_sims.update(cos)

    # Vector arm: rank only docs that clear the absolute floor.
    order = np.argsort(-sims)
    vec_ids = [ids[i] for i in order[:candidates] if sims[i] >= floor]
    event_ids = {r["id"] for r in db.execute("SELECT id FROM docs WHERE kind='event'")}
    if dates:
        # A question about a specific day is answered by the date filter above or
        # not at all. Similarity must not offer a different day's event instead:
        # "nothing is scheduled" is a real answer, and a plausible-looking wrong
        # date is worse than silence.
        vec_ids = [i for i in vec_ids if i not in event_ids]
    elif tense == "future":
        vec_ids = [i for i in vec_ids if i not in event_ids or i in _future_ids]
    elif tense == "past":
        past_ids = {r["id"] for r in db.execute(
            "SELECT id FROM docs WHERE kind='event' AND SUBSTR(ts,1,10) <= ?", (today,))}
        vec_ids = [i for i in vec_ids if i not in event_ids or i in past_ids]
    vec_rank = {doc_id: i for i, doc_id in enumerate(vec_ids)}

    # Lexical arm: skipped entirely when the query has no content-bearing terms.
    lex_rank = {}
    match = _fts_query(" ".join(segs[:3]) if distilled else query, stopwords=stopwords)
    if match:
        where = " AND docs.source=?" if source else ""
        args = [match] + ([source] if source else []) + [candidates]
        try:
            lexical = db.execute(
                f"""SELECT docs.id FROM docs_fts JOIN docs ON docs.id = docs_fts.rowid
                    WHERE docs_fts MATCH ?{where} ORDER BY docs_fts.rank LIMIT ?""",
                args,
            ).fetchall()
        except sqlite3.OperationalError:
            lexical = []
        # A BM25 hit still has to be semantically plausible. Without this a doc
        # sharing one rare-ish word rides the lexical arm into the results.
        allowed = set(vec_ids) | {i for i in cos if i not in event_ids}
        if dates:
            allowed -= event_ids
        lex_rank = {r["id"]: i for i, r in enumerate(lexical)
                    if cos.get(r["id"], 0.0) >= floor and r["id"] in allowed}

    now = datetime.now(timezone.utc)
    rec_cfg = _recency_cfg()
    import config as _c
    use_cfg = _c.load().get("usage", {})
    uses = usage_index(db, use_cfg) if use_cfg.get("enabled", True) else {}
    fused = []
    for doc_id in set(lex_rank) | set(vec_rank):
        score = 0.0
        if doc_id in lex_rank:
            score += 1.0 / (rrf_k + lex_rank[doc_id])
        if doc_id in vec_rank:
            score += 1.0 / (rrf_k + vec_rank[doc_id])
        row = db.execute("SELECT * FROM docs WHERE id=?", (doc_id,)).fetchone()
        weight = (_age_factor(row["ts"], now, row["kind"], rec_cfg)
                  * _usage_boost(uses.get(doc_id), use_cfg))
        # A nudge, not a filter. "What did I decide" should surface decisions
        # first, but a fact that answers it better must still be able to win.
        if want_kinds and row["kind"] in want_kinds:
            weight *= float(cfg.get("kind_boost", 1.35))
        # Confidence, where recorded. Something stated outright outranks
        # something inferred; absent means no opinion, so no adjustment.
        try:
            conf = json.loads(row["metadata"] or "{}").get("confidence")
            if isinstance(conf, (int, float)):
                weight *= 0.6 + 0.4 * max(0.0, min(1.0, float(conf)))
        except ValueError:
            pass
        fused.append((score * weight, row))
    fused.sort(key=lambda x: -x[0])

    # Greedy selection with a per-source cap and near-duplicate suppression.
    pos = {doc_id: i for i, doc_id in enumerate(ids)}
    def _parent(row):
        try:
            return json.loads(row["metadata"] or "{}").get("parent")
        except ValueError:
            return None

    picked, chosen_vecs, per_source, seen_titles = [], [], {}, set()
    per_parent = {}
    for sc, row in dated[:limit]:        # exact date matches always lead
        picked.append((sc, row))
        seen_titles.add(_norm_title(row["title"]))
        per_source[row["source"]] = per_source.get(row["source"], 0) + 1
    for score, row in fused:
        if len(picked) >= limit:
            break
        src = row["source"]
        if per_source.get(src, 0) >= max_per_source:
            continue
        title = _norm_title(row["title"])
        if title and title in seen_titles:
            continue
        # Chunks of one document are separate rows but one source. Without a cap
        # a single long note answers a question six times over.
        par = _parent(row)
        if par is not None:
            if per_parent.get(par, 0) >= max_per_parent:
                continue
            per_parent[par] = per_parent.get(par, 0) + 1
        if mmr > 0 and chosen_vecs:
            v = mat[pos[row["id"]]]
            if max(float(v @ c) for c in chosen_vecs) >= mmr:
                continue
            chosen_vecs.append(v)
        elif mmr > 0:
            chosen_vecs.append(mat[pos[row["id"]]])
        per_source[src] = per_source.get(src, 0) + 1
        seen_titles.add(title)
        picked.append((score, row))
        if len(picked) >= limit:
            break
    if log and picked:
        qjson = json.dumps({"queries": segs, "dates": dates, "tense": tense})
        log_retrievals(db, [
            {"id": row["id"], "sim": cos.get(row["id"]), "rank": i, "query": query[:2000],
             "queries": qjson, "session": session, "agent": agent}
            for i, (_, row) in enumerate(picked, 1)
        ])
    return picked


def stats(db):
    return db.execute(
        "SELECT source, COUNT(*) AS n, SUM(embedding IS NOT NULL) AS embedded, MAX(ts) AS newest FROM docs GROUP BY source"
    ).fetchall()


def kind_stats(db):
    return db.execute(
        """SELECT COALESCE(kind,'(untyped)') AS kind, COUNT(*) AS n, MAX(ts) AS newest
           FROM docs GROUP BY COALESCE(kind,'(untyped)') ORDER BY n DESC"""
    ).fetchall()
