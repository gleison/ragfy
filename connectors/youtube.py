"""Ingest explicitly selected YouTube video transcripts."""
import re
from datetime import datetime, timezone

import config


VIDEO_ID = re.compile(r"(?:youtu\.be/|youtube\.com/(?:watch\?v=|shorts/))([A-Za-z0-9_-]{11})(?:[?&/]|$)")


def _video_id(url):
    match = VIDEO_ID.search(url)
    return match.group(1) if match else None


def _fetch_transcript(video_id):
    try:
        from youtube_transcript_api import YouTubeTranscriptApi
    except ImportError as error:
        raise RuntimeError("youtube-transcript-api is not installed. Run pip install youtube-transcript-api") from error
    transcript = YouTubeTranscriptApi().fetch(video_id)
    return " ".join(item.text for item in transcript if item.text).strip()


def fetch(urls=None):
    cfg = config.load().get("youtube", {})
    urls = urls if urls is not None else cfg.get("urls", [])
    docs = []
    for raw_url in urls:
        video_id = _video_id(str(raw_url))
        if not video_id:
            print(f"  youtube: skipped invalid video URL: {raw_url}")
            continue
        try:
            text = _fetch_transcript(video_id)
        except Exception as error:
            print(f"  youtube[{video_id}]: transcript unavailable ({str(error)[:120]})")
            continue
        if len(text) < 40:
            print(f"  youtube[{video_id}]: transcript was empty")
            continue
        url = f"https://www.youtube.com/watch?v={video_id}"
        docs.append({
            "source": "youtube",
            "source_id": video_id,
            "ts": datetime.now(timezone.utc).isoformat(),
            "title": f"YouTube video {video_id}",
            "content": text,
            "kind": "video-transcript",
            "area": "youtube",
            "metadata": {"url": url, "video_id": video_id},
        })
    print(f"  youtube: {len(docs)} video transcript(s)")
    return docs
