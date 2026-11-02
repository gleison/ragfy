"""Small LLM helper for the curator's audit and `kb.py ask`.

Uses the same OpenAI key the embeddings already need, so installing this system
requires exactly one credential. Everything here is cheap: `gpt-4o-mini` at a
few hundred tokens a call, a handful of calls a week.
"""
import os


def complete(prompt, model=None, max_tokens=1000):
    from openai import OpenAI

    model = model or os.environ.get("KB_LLM_MODEL", "gpt-4o-mini")
    return OpenAI().chat.completions.create(
        model=model,
        messages=[{"role": "user", "content": prompt}],
        temperature=0,
        max_tokens=max_tokens,
    ).choices[0].message.content.strip()
