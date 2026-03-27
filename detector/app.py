"""
Minimal detector sidecar for local development.

Go API forwards JPEG bytes to POST /internal/detect (see backend/cmd/api/detector_client.go).

Replace the stub response with YOLO / Ultralytics inference when ready.
"""

from __future__ import annotations

import io
import time
from typing import Any

from fastapi import FastAPI, Query, Request
from fastapi.responses import JSONResponse
from PIL import Image

app = FastAPI(title="Live Traffic Detector", version="0.1.0")


@app.get("/healthz")
def healthz() -> dict[str, Any]:
    return {"ok": True, "service": "detector-sidecar"}


@app.post("/internal/detect")
async def internal_detect(
    request: Request,
    stream_id: str | None = Query(None, description="Logical stream / camera id"),
    imgsz: int = Query(640, ge=32, le=4096),
    conf: float = Query(0.25, ge=0.0, le=1.0),
) -> dict[str, Any]:
    body = await request.body()
    if not body:
        return JSONResponse(
            status_code=400,
            content={"error": "request body must contain image bytes"},
        )

    t0 = time.perf_counter()
    width, height = 0, 0
    try:
        img = Image.open(io.BytesIO(body))
        width, height = img.size
    except Exception:
        pass

    # Stub: empty detections. Swap for YOLO output (boxes, scores, class names).
    latency_ms = (time.perf_counter() - t0) * 1000.0
    return {
        "stream_id": stream_id or "",
        "imgsz": imgsz,
        "conf": conf,
        "image": {"width": width, "height": height},
        "detections": [],
        "metrics": {
            "latency_ms": round(latency_ms, 2),
            "stub": True,
        },
    }
