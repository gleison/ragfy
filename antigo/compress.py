"""Shrink a session transcript before it reaches the distiller.

The nightly ingest was burning 20-30% of Mason's session allowance because
every settled session gets handed to a model in full. Most of that text carries
no knowledge: acknowledgements, restated status, repeated paths, long code
blocks, and the same sentence said three ways.

This is deliberately DETERMINISTIC. A model deciding what to cut is another
model call, which is the cost being removed, and it could drop the one line
that mattered. Every rule here removes text that is provably redundant (an
exact repeat) or provably contentless (a bare "ok"), and nothing else.

  from compress import compress
  small, stats = compress(spine)
"""
import re

ACK = re.compile(
    r"^(ok(ay)?|k|yes|yep|yeah|sure|thanks?|thank you|ty|got it|sounds good|"
    r"perfect|great|nice|cool|awesome|continue|go ahead|keep going|proceed|"
    r"do it|please|np|no problem|right|correct|exactly|agreed|makes sense|"
    r"understood|good|fine|alright|k thanks|lgtm|ship it)"
    r"[\s.!,]*$", re.I)

# Assistant status narration that restates the obvious. Removing these cannot
# lose a decision, a preference, or a fix.
NARRATION = re.compile(
    r"^(let me |i'?ll |i am going to |i'?m going to |now i |next,? i |"
    r"perfect[.!] |great[.!] |done[.!] |all set|here'?s what i|"
    r"i'?ve (now |just )?(finished|completed|updated|created|added))",
    re.I)

FENCE = re.compile(r"^\s*```")
NOISE_LINE = re.compile(r"^\s*([-=*_#]{3,}|\.{3,})\s*$")
PATHISH = re.compile(r"(/Users/[^\s\"']{20,}|[0-9a-f]{32,}|data:[a-z]+/[a-z]+;base64,[A-Za-z0-9+/=]+)")
WS = re.compile(r"[ \t]{2,}")

CODE_HEAD, CODE_TAIL = 6, 3
MAX_MSG_LINES = 40
MSG_HEAD, MSG_TAIL = 22, 10


def _squeeze_code(lines):
    """Keep the shape of a code block, not all of it. The distiller is asked for
    decisions and fixes, never to reproduce a file."""
    out, i = [], 0
    while i < len(lines):
        if FENCE.match(lines[i]):
            j = i + 1
            while j < len(lines) and not FENCE.match(lines[j]):
                j += 1
            block = lines[i + 1:j]
            if len(block) > CODE_HEAD + CODE_TAIL + 2:
                out.append(lines[i])
                out += block[:CODE_HEAD]
                out.append(f"    … {len(block) - CODE_HEAD - CODE_TAIL} lines elided …")
                out += block[-CODE_TAIL:]
                out.append("```")
            else:
                out += lines[i:j + 1]
            i = j + 1
        else:
            out.append(lines[i])
            i += 1
    return out


def _trim_message(body):
    lines = body.split("\n")
    lines = _squeeze_code(lines)
    if len(lines) > MAX_MSG_LINES:
        lines = (lines[:MSG_HEAD]
                 + [f"    … {len(lines) - MSG_HEAD - MSG_TAIL} lines elided …"]
                 + lines[-MSG_TAIL:])
    return "\n".join(lines)


def compress(spine, dedupe=True):
    """Returns (compressed_text, stats)."""
    before = len(spine)
    messages = [m for m in spine.split("\n\n") if m.strip()]
    seen, out = set(), []

    for msg in messages:
        speaker, _, body = msg.partition(": ")
        if speaker not in ("USER", "CLAUDE"):
            speaker, body = "", msg
        body = body.strip()
        if not body:
            continue
        if ACK.match(body):
            continue                       # a bare acknowledgement holds nothing
        body = "\n".join(l for l in body.split("\n") if not NOISE_LINE.match(l))
        body = PATHISH.sub(lambda m: m.group(0)[:40] + "…", body)
        body = WS.sub(" ", body)
        body = _trim_message(body)
        if speaker == "CLAUDE":
            kept = [l for l in body.split("\n")
                    if not (NARRATION.match(l.strip()) and len(l.strip()) < 120)]
            body = "\n".join(kept).strip()
        if not body:
            continue
        if dedupe:
            key = re.sub(r"\W+", "", body.lower())[:400]
            if key and key in seen:
                continue                   # the same thing, said again
            seen.add(key)
        out.append(f"{speaker}: {body}" if speaker else body)

    text = "\n\n".join(out)
    after = len(text)
    return text, {"before": before, "after": after,
                  "pct": round(100 * after / before, 1) if before else 100.0,
                  "messages_in": len(messages), "messages_out": len(out)}
