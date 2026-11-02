#!/usr/bin/env python3
"""chunk-review — grade the knowledge base on what it actually served you.

Opens a page showing, for the current session: every prompt, what the distiller
broke it into, and each chunk that got injected. Mark each chunk good or junk.
Verdicts write straight back to the retrievals table.

Why this and not only the automatic audit: the audit is a model grading a
model's retrieval, so it shares blind spots with the thing it grades and can
only guess what was meant. Mason knows what he meant and whether the chunk
helped. His verdict overrides the audit's, and a chunk he calls junk stops
earning usage credit immediately.

  python chunk_review.py                 # this session (newest)
  python chunk_review.py --session ID
  python chunk_review.py --hours 24      # everything served today
  python chunk_review.py --no-open
"""
import argparse
import html
import json
import sys
import threading
import webbrowser
from collections import OrderedDict
from datetime import datetime, timedelta, timezone
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

KB = Path(__file__).resolve().parent
sys.path.insert(0, str(KB))
import config  # noqa: F401
import store

CSS = """
:root{--bg:#fbfbfa;--fg:#1c1b19;--mut:#6b6862;--line:#e3e0da;--card:#fff;
      --good:#1a7f4b;--goodbg:#e7f5ed;--bad:#b3261e;--badbg:#fdeceb;--code:#f4f2ee;--accent:#1c1b19}
@media(prefers-color-scheme:dark){:root{--bg:#161513;--fg:#eceae5;--mut:#9a958c;--line:#2e2c28;
      --card:#1e1d1a;--good:#4ade80;--goodbg:#14301f;--bad:#f87171;--badbg:#331617;--code:#232120;--accent:#eceae5}}
*{box-sizing:border-box}
body{margin:0;padding:28px 20px 120px;background:var(--bg);color:var(--fg);
     font:15px/1.6 ui-sans-serif,-apple-system,"SF Pro Text",Segoe UI,sans-serif}
.wrap{max-width:940px;margin:0 auto}
h1{font-size:21px;margin:0 0 3px;letter-spacing:-.01em}
.sub{color:var(--mut);font-size:13px;margin-bottom:26px}
.p{background:var(--card);border:1px solid var(--line);border-radius:11px;padding:18px 20px;margin-bottom:16px}
.pt{font-weight:600;font-size:15.5px;margin-bottom:9px}
.qs{background:var(--code);border-radius:7px;padding:9px 12px;margin-bottom:13px;font-size:12.5px;color:var(--mut)}
.qs b{color:var(--fg)}
.qs ul{margin:4px 0 0;padding-left:17px}
.none{color:var(--bad);font-weight:600;font-size:13px}
.c{border-top:1px solid var(--line);padding:12px 0 8px}
.ch{display:flex;gap:8px;align-items:center;flex-wrap:wrap;margin-bottom:5px}
.k{font:600 10px/1 ui-monospace,Menlo,monospace;text-transform:uppercase;letter-spacing:.05em;
   padding:3px 6px;border-radius:4px;background:var(--code);color:var(--mut)}
.sim{font:600 11.5px/1 ui-monospace,Menlo,monospace;color:var(--mut)}
.ct{font-weight:600;font-size:13.5px;margin:3px 0 5px}
pre{background:var(--code);border-radius:7px;padding:10px 12px;margin:0 0 8px;
    font:12px/1.5 ui-monospace,Menlo,monospace;white-space:pre-wrap;word-break:break-word}
.btns{display:flex;gap:7px}
button{font:600 12px/1 inherit;padding:7px 13px;border-radius:6px;border:1px solid var(--line);
       background:var(--card);color:var(--fg);cursor:pointer}
button:hover{border-color:var(--mut)}
.c.g{background:var(--goodbg);border-radius:8px;padding-left:11px}
.c.b{background:var(--badbg);border-radius:8px;padding-left:11px}
.c.g .yes{background:var(--good);color:#fff;border-color:var(--good)}
.c.b .no{background:var(--bad);color:#fff;border-color:var(--bad)}
.bar{position:fixed;left:0;right:0;bottom:0;background:var(--card);border-top:1px solid var(--line);
     padding:13px 20px;display:flex;gap:14px;align-items:center;justify-content:center}
.bar span{color:var(--mut);font-size:13px}
.save{background:var(--accent);color:var(--bg);border-color:var(--accent);padding:9px 20px;font-size:13px}
"""

JS = """
const V={};
function mark(el,id,v){const c=el.closest('.c');c.classList.remove('g','b');
  if(V[id]===v){delete V[id];}else{V[id]=v;c.classList.add(v?'g':'b');}
  document.getElementById('n').textContent=Object.keys(V).length;}
async function save(){
  const r=await fetch('/save',{method:'POST',headers:{'Content-Type':'application/json'},
    body:JSON.stringify(V)});
  const j=await r.json();
  const tune=j.optimization&&j.optimization.message?` ${j.optimization.message}`:'';
  document.getElementById('msg').textContent=j.ok?`Saved ${j.n} verdicts.${tune}`:'Save failed.';
}
"""


def _fmt(dt):
    try:
        d = datetime.fromisoformat(dt.replace("Z", "+00:00"))
        return d.astimezone().strftime("%-I:%M %p")
    except Exception:
        return ""


def build(db, session=None, hours=None):
    where, args = [], []
    if session:
        where.append("r.session = ?")
        args.append(session)
    if hours:
        cutoff = (datetime.now(timezone.utc) - timedelta(hours=hours)).isoformat()
        where.append("r.ts >= ?")
        args.append(cutoff)
    sql = ("SELECT r.rowid rid, r.ts, r.query, r.queries, r.sim, r.rank, r.verdict, "
           "r.source_agent, d.title, d.content, d.kind, d.source, d.id doc_id "
           "FROM retrievals r LEFT JOIN docs d ON d.id = r.doc_id")
    if where:
        sql += " WHERE " + " AND ".join(where)
    sql += " ORDER BY r.ts ASC, r.rank ASC"
    rows = db.execute(sql, args).fetchall()

    groups = OrderedDict()
    for r in rows:
        key = (r["ts"][:19], r["query"] or "")
        groups.setdefault(key, {"queries": r["queries"], "agent": r["source_agent"],
                                "ts": r["ts"], "hits": []})["hits"].append(r)

    out = [f"<style>{CSS}</style><script>{JS}</script><div class=wrap>",
           "<h1>Was this actually relevant?</h1>",
           f"<div class=sub>{len(groups)} prompt(s) &middot; {len(rows)} chunks served &middot; "
           "mark anything that did not help. Unmarked chunks stay unjudged.</div>"]

    for (ts, prompt), g in groups.items():
        out.append("<div class=p>")
        agent = f" &middot; {html.escape(g['agent'])}" if g["agent"] else ""
        out.append(f"<div class=pt>{html.escape(prompt[:400])}</div>")
        out.append(f"<div class=sub style='margin:0 0 10px'>{_fmt(g['ts'])}{agent}</div>")
        try:
            qd = json.loads(g["queries"] or "{}")
        except ValueError:
            qd = {}
        if qd.get("queries"):
            items = "".join(f"<li>{html.escape(str(q))}</li>" for q in qd["queries"])
            extra = ""
            if qd.get("dates"):
                extra = f" &middot; dates: {html.escape(', '.join(qd['dates']))}"
            if qd.get("tense"):
                extra += f" &middot; tense: {html.escape(qd['tense'])}"
            out.append(f"<div class=qs><b>Searched as:</b>{extra}<ul>{items}</ul></div>")
        for r in g["hits"]:
            cls = "" if r["verdict"] is None else (" g" if r["verdict"] else " b")
            out.append(f"<div class='c{cls}'>")
            out.append(f"<div class=ch><span class=k>{html.escape(r['kind'] or r['source'] or '?')}</span>"
                       f"<span class=sim>sim {(r['sim'] or 0):.3f} &middot; rank {r['rank']}</span></div>")
            out.append(f"<div class=ct>{html.escape(r['title'] or '(missing)')}</div>")
            out.append(f"<pre>{html.escape((r['content'] or '')[:900])}</pre>")
            out.append(f"<div class=btns>"
                       f"<button class=yes onclick=\"mark(this,{r['rid']},1)\">Useful</button>"
                       f"<button class=no onclick=\"mark(this,{r['rid']},0)\">Junk</button></div>")
            out.append("</div>")
        out.append("</div>")

    out.append("</div><div class=bar><span><b id=n>0</b> marked</span>"
               "<button class=save onclick=save()>Save verdicts</button>"
               "<span id=msg></span></div>")
    return "\n".join(out), len(groups), len(rows)


def serve(page, db_path, port=8737, open_browser=True):
    saved = {"n": 0}

    class H(BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def do_GET(self):
            body = page.encode()
            self.send_response(200)
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def do_POST(self):
            n = int(self.headers.get("Content-Length", 0))
            try:
                verdicts = json.loads(self.rfile.read(n).decode())
                db = store.connect()
                # Marked human so the weekly audit never overwrites it, and so
                # the curator can treat a person's "junk" as decisive where a
                # model's needs corroboration.
                db.executemany(
                    "UPDATE retrievals SET verdict=?, judged_by='human' WHERE rowid=?",
                    [(int(v), int(k)) for k, v in verdicts.items()])
                db.commit()
                saved["n"] += len(verdicts)
                from relevance_optimizer import optimize_from_feedback
                optimization = optimize_from_feedback(db)
                out = json.dumps({"ok": True, "n": len(verdicts),
                                  "optimization": optimization}).encode()
            except Exception as e:
                out = json.dumps({"ok": False, "error": str(e)[:200]}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(out)))
            self.end_headers()
            self.wfile.write(out)

    srv = HTTPServer(("127.0.0.1", port), H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    url = f"http://127.0.0.1:{port}/"
    print(f"chunk-review open at {url}   (Ctrl-C when done)")
    if open_browser:
        webbrowser.open(url)
    try:
        threading.Event().wait()
    except KeyboardInterrupt:
        print(f"\nsaved {saved['n']} verdicts")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--session")
    ap.add_argument("--hours", type=float)
    ap.add_argument("--port", type=int, default=8737)
    ap.add_argument("--no-open", action="store_true")
    ap.add_argument("--html-only")
    args = ap.parse_args()

    db = store.connect()
    session = args.session
    if not session and not args.hours:
        row = db.execute("SELECT session FROM retrievals WHERE session IS NOT NULL "
                         "ORDER BY ts DESC LIMIT 1").fetchone()
        session = row["session"] if row else None
        if not session:
            args.hours = 24
    page, nq, nc = build(db, session, args.hours)
    print(f"{nq} prompt(s), {nc} chunks"
          + (f"  session {session[:8]}…" if session else f"  last {args.hours}h"))
    if nc == 0:
        print("nothing served yet in that scope — send a prompt or two first")
        return
    if args.html_only:
        Path(args.html_only).write_text(page)
        print(f"wrote {args.html_only}")
        return
    serve(page, store.DB_PATH, args.port, not args.no_open)


if __name__ == "__main__":
    main()
