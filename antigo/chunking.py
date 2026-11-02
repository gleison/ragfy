"""Split long documents into separately-retrievable pieces.

The KB stores distilled session records, which are already one-idea-sized, so
nothing needed chunking. PDFs and long notes are the opposite: a 30-page paper
embedded as a single vector is the blurry-average problem — it sits near
everything and close to nothing, matching weakly but constantly. 167 of Mason's
existing notes are already over 3,000 characters, one at the 20,000 cap.

Splits on structure first (blank lines, then sentences) so a chunk is a coherent
passage rather than a fixed-width slice. Overlap carries a little context across
the seam, so a fact stated at a boundary is still findable from either side.
"""
import re

TARGET_TOKENS = 450
OVERLAP_TOKENS = 60
MIN_TOKENS = 60
CHARS_PER_TOKEN = 4          # close enough for splitting; exact counts cost a tokenizer call

PARA = re.compile(r"\n\s*\n")
SENT = re.compile(r"(?<=[.!?])\s+(?=[A-Z0-9])")


def _tok(text):
    return max(1, len(text) // CHARS_PER_TOKEN)


def _split_oversized(block, target):
    """A single paragraph longer than the target still has to come apart."""
    if _tok(block) <= target:
        return [block]
    out, cur = [], ""
    for sent in SENT.split(block):
        if cur and _tok(cur) + _tok(sent) > target:
            out.append(cur.strip())
            cur = sent
        else:
            cur = f"{cur} {sent}".strip()
    if cur.strip():
        out.append(cur.strip())
    # A sentence longer than the target (tables, minified code) gets cut by length.
    final = []
    for piece in out:
        if _tok(piece) <= target * 1.5:
            final.append(piece)
        else:
            step = target * CHARS_PER_TOKEN
            final += [piece[i:i + step] for i in range(0, len(piece), step)]
    return final


def chunk_text(text, target=TARGET_TOKENS, overlap=OVERLAP_TOKENS, min_tokens=MIN_TOKENS):
    """Return a list of passages. Short text comes back as a single chunk."""
    text = (text or "").strip()
    if not text:
        return []
    if _tok(text) <= target:
        return [text]

    blocks = []
    for para in PARA.split(text):
        para = para.strip()
        if para:
            blocks += _split_oversized(para, target)

    chunks, cur = [], ""
    for block in blocks:
        if cur and _tok(cur) + _tok(block) > target:
            chunks.append(cur.strip())
            tail = cur[-(overlap * CHARS_PER_TOKEN):]
            # Start the overlap at a sentence boundary when there is one nearby.
            m = SENT.search(tail)
            cur = (tail[m.end():] if m else tail).strip() + "\n\n" + block
        else:
            cur = f"{cur}\n\n{block}".strip() if cur else block
    if cur.strip():
        chunks.append(cur.strip())

    # Fold a runt tail into its predecessor rather than storing a fragment.
    if len(chunks) > 1 and _tok(chunks[-1]) < min_tokens:
        chunks[-2] = chunks[-2] + "\n\n" + chunks.pop()
    return chunks


def chunk_doc(doc, threshold=TARGET_TOKENS, target=TARGET_TOKENS, overlap=OVERLAP_TOKENS):
    """One connector doc -> one or more docs.

    Chunks keep the parent's title (so a hit still says what it came from) and
    carry `parent`/`chunk_ix` in metadata, which retrieval uses to stop a single
    long document from filling every slot.
    """
    content = doc.get("content") or ""
    if _tok(content) <= threshold:
        return [doc]
    pieces = chunk_text(content, target, overlap)
    if len(pieces) <= 1:
        return [doc]
    out = []
    base_id = doc["source_id"]
    title = doc.get("title") or ""
    for i, piece in enumerate(pieces):
        d = dict(doc)
        d["source_id"] = f"{base_id}#{i}"
        d["content"] = piece
        d["title"] = f"{title} ({i + 1}/{len(pieces)})" if title else f"part {i + 1}"
        meta = dict(doc.get("metadata") or {})
        meta.update({"parent": base_id, "chunk_ix": i, "chunks": len(pieces),
                     "parent_title": title})
        d["metadata"] = meta
        out.append(d)
    return out
