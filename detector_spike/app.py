from __future__ import annotations

import os
import threading
import time
from dataclasses import dataclass

import cv2
import numpy as np
from fastapi import FastAPI, HTTPException, Query, Request
from ultralytics import YOLO

TARGET_CLASSES = {"car", "truck", "bus", "motorcycle"}


@dataclass
class TrackState:
    track_id: int
    cx: float
    cy: float
    last_ts: float


class StreamTracker:
    """Tiny nearest-neighbor tracker for a detector spike."""

    def __init__(self, ttl_sec: float = 4.0, max_match_px: float = 70.0) -> None:
        self.ttl_sec = ttl_sec
        self.max_match_px = max_match_px
        self._lock = threading.Lock()
        self._next_track_id = 1
        self._streams: dict[str, dict[int, TrackState]] = {}

    def _prune_stale(self, tracks: dict[int, TrackState], now_ts: float) -> None:
        stale = [tid for tid, t in tracks.items() if now_ts-t.last_ts > self.ttl_sec]
        for tid in stale:
            del tracks[tid]

    def update(self, stream_id: str, centroids: list[tuple[float, float]], now_ts: float) -> list[dict]:
        with self._lock:
            stream_tracks = self._streams.setdefault(stream_id, {})
            self._prune_stale(stream_tracks, now_ts)
            used_track_ids: set[int] = set()
            out: list[dict] = []

            for cx, cy in centroids:
                best_tid = None
                best_dist = float("inf")
                best_prev = None
                for tid, prev in stream_tracks.items():
                    if tid in used_track_ids:
                        continue
                    dist = ((cx - prev.cx) ** 2 + (cy - prev.cy) ** 2) ** 0.5
                    if dist < best_dist:
                        best_dist = dist
                        best_tid = tid
                        best_prev = prev

                speed_px_s = 0.0
                if best_tid is not None and best_prev is not None and best_dist <= self.max_match_px:
                    dt = max(1e-3, now_ts - best_prev.last_ts)
                    speed_px_s = best_dist / dt
                    stream_tracks[best_tid] = TrackState(
                        track_id=best_tid,
                        cx=cx,
                        cy=cy,
                        last_ts=now_ts,
                    )
                    track_id = best_tid
                    used_track_ids.add(best_tid)
                else:
                    track_id = self._next_track_id
                    self._next_track_id += 1
                    stream_tracks[track_id] = TrackState(
                        track_id=track_id,
                        cx=cx,
                        cy=cy,
                        last_ts=now_ts,
                    )
                    used_track_ids.add(track_id)

                out.append({"track_id": track_id, "speed_px_s": speed_px_s})

            return out


def _parse_roi_points(raw_roi: str, width: int, height: int) -> np.ndarray:
    if not raw_roi.strip():
        return np.array([[0, 0], [width - 1, 0], [width - 1, height - 1], [0, height - 1]], dtype=np.int32)

    parts = [p.strip() for p in raw_roi.split(",") if p.strip()]
    if len(parts) < 6 or len(parts) % 2 != 0:
        raise ValueError("roi must have x,y pairs for at least 3 points")

    coords = []
    for i in range(0, len(parts), 2):
        x = int(max(0, min(width - 1, float(parts[i]))))
        y = int(max(0, min(height - 1, float(parts[i + 1]))))
        coords.append([x, y])
    return np.array(coords, dtype=np.int32)


def _occupancy_ratio(detections: list[dict], roi_poly: np.ndarray, width: int, height: int) -> tuple[int, int, float]:
    roi_mask = np.zeros((height, width), dtype=np.uint8)
    cv2.fillPoly(roi_mask, [roi_poly], 1)
    roi_area_px = int(np.count_nonzero(roi_mask))

    occupied = np.zeros((height, width), dtype=np.uint8)
    for d in detections:
        x1, y1, x2, y2 = d["bbox"]
        xx1 = max(0, min(width - 1, int(x1)))
        yy1 = max(0, min(height - 1, int(y1)))
        xx2 = max(0, min(width, int(x2)))
        yy2 = max(0, min(height, int(y2)))
        if xx2 > xx1 and yy2 > yy1:
            occupied[yy1:yy2, xx1:xx2] = 1

    occupied_px = int(np.count_nonzero((occupied == 1) & (roi_mask == 1)))
    ratio = float(occupied_px / roi_area_px) if roi_area_px > 0 else 0.0
    return occupied_px, roi_area_px, ratio


def _load_model() -> tuple[YOLO, str]:
    model_name = os.getenv("DETECTOR_MODEL", "yolov8n.pt").strip() or "yolov8n.pt"
    return YOLO(model_name), model_name


app = FastAPI(title="Traffic Detector Spike", version="0.1.0")
model, model_name = _load_model()
device = os.getenv("DETECTOR_DEVICE", "cpu").strip() or "cpu"
tracker = StreamTracker(
    ttl_sec=float(os.getenv("DETECTOR_TRACK_TTL_SEC", "4.0")),
    max_match_px=float(os.getenv("DETECTOR_TRACK_MAX_MATCH_PX", "70.0")),
)
inference_lock = threading.Lock()


@app.get("/internal/health")
def health() -> dict:
    return {"ok": True, "model": model_name, "device": device}


@app.post("/internal/detect")
async def detect(
    request: Request,
    stream_id: str = Query("default"),
    conf: float = Query(0.25, ge=0.01, le=0.95),
    iou: float = Query(0.45, ge=0.05, le=0.95),
    imgsz: int = Query(640, ge=160, le=1280),
    roi: str = Query(default=""),
    moving_speed_threshold_px_s: float = Query(12.0, ge=0.1, le=2000.0),
) -> dict:
    image_bytes = await request.body()
    if not image_bytes:
        raise HTTPException(status_code=400, detail="request body must contain JPEG/PNG bytes")

    np_bytes = np.frombuffer(image_bytes, dtype=np.uint8)
    image = cv2.imdecode(np_bytes, cv2.IMREAD_COLOR)
    if image is None:
        raise HTTPException(status_code=400, detail="failed to decode image bytes")
    height, width = image.shape[:2]

    try:
        roi_poly = _parse_roi_points(roi, width, height)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc

    t0 = time.perf_counter()
    with inference_lock:
        results = model.predict(
            source=image,
            conf=conf,
            iou=iou,
            imgsz=imgsz,
            device=device,
            verbose=False,
        )
    inference_ms = (time.perf_counter() - t0) * 1000.0

    names = results[0].names
    detections: list[dict] = []
    centroids: list[tuple[float, float]] = []

    boxes = results[0].boxes
    if boxes is not None:
        for box in boxes:
            cls_id = int(box.cls.item())
            class_name = str(names.get(cls_id, cls_id))
            if class_name not in TARGET_CLASSES:
                continue
            x1, y1, x2, y2 = box.xyxy[0].tolist()
            confidence = float(box.conf.item())
            cx = float((x1 + x2) / 2.0)
            cy = float((y1 + y2) / 2.0)
            centroids.append((cx, cy))
            detections.append(
                {
                    "bbox": [x1, y1, x2, y2],
                    "class_id": cls_id,
                    "class_name": class_name,
                    "confidence": confidence,
                    "center": [cx, cy],
                }
            )

    tracking = tracker.update(stream_id=stream_id, centroids=centroids, now_ts=time.time())
    moving_count = 0
    for i, t in enumerate(tracking):
        detections[i]["track_id"] = t["track_id"]
        detections[i]["speed_px_s"] = t["speed_px_s"]
        detections[i]["is_moving"] = bool(t["speed_px_s"] >= moving_speed_threshold_px_s)
        if detections[i]["is_moving"]:
            moving_count += 1

    occupied_px, roi_area_px, occupancy_ratio = _occupancy_ratio(
        detections=detections,
        roi_poly=roi_poly,
        width=width,
        height=height,
    )
    total_tracks = len(detections)
    stationary_count = max(0, total_tracks - moving_count)
    moving_ratio = float(moving_count / total_tracks) if total_tracks > 0 else 0.0
    mean_speed = float(sum(d["speed_px_s"] for d in detections) / total_tracks) if total_tracks > 0 else 0.0

    return {
        "model": model_name,
        "device": device,
        "inference_ms": round(inference_ms, 2),
        "image": {"width": width, "height": height},
        "detections": detections,
        "metrics": {
            "occupancy_ratio": occupancy_ratio,
            "occupied_px": occupied_px,
            "roi_area_px": roi_area_px,
            "vehicle_count": total_tracks,
            "moving_vehicle_count": moving_count,
            "stationary_vehicle_count": stationary_count,
            "moving_ratio": moving_ratio,
            "mean_track_speed_px_s": mean_speed,
        },
        "metric_definitions": {
            "occupancy_ratio": "fraction of ROI pixels covered by union of vehicle detection boxes",
            "moving_ratio": "moving vehicles / total vehicles from nearest-neighbor track updates",
            "mean_track_speed_px_s": "mean centroid speed in pixels/sec across tracked vehicles",
        },
        "stream_id": stream_id,
        "ts_unix_ms": int(time.time() * 1000),
    }
