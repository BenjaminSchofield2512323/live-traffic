from __future__ import annotations

import base64
import json
import os
import threading
import time
from collections import deque
from dataclasses import dataclass, field
from typing import Any

import cv2
import numpy as np
from fastapi import FastAPI, HTTPException, Query, Request
from ultralytics import YOLO

TARGET_CLASSES = {"car", "truck", "bus", "motorcycle"}


def _env_float(key: str, default: float) -> float:
    raw = os.getenv(key, "").strip()
    if not raw:
        return default
    try:
        return float(raw)
    except ValueError:
        return default


def _env_int(key: str, default: int) -> int:
    raw = os.getenv(key, "").strip()
    if not raw:
        return default
    try:
        return int(raw)
    except ValueError:
        return default


def _env_bool(key: str, default: bool) -> bool:
    raw = os.getenv(key, "").strip().lower()
    if not raw:
        return default
    return raw in {"1", "true", "yes", "y", "on"}


def _safe_norm(dx: float, dy: float) -> tuple[float, float]:
    mag = (dx * dx + dy * dy) ** 0.5
    if mag <= 1e-6:
        return (1.0, 0.0)
    return (dx / mag, dy / mag)


def _parse_roi_points(raw_roi: str, width: int, height: int) -> np.ndarray:
    if not raw_roi.strip():
        return np.array([[0, 0], [width - 1, 0], [width - 1, height - 1], [0, height - 1]], dtype=np.int32)
    parts = [p.strip() for p in raw_roi.split(",") if p.strip()]
    if len(parts) < 6 or len(parts) % 2 != 0:
        raise ValueError("roi must have x,y pairs for at least 3 points")
    coords: list[list[int]] = []
    for i in range(0, len(parts), 2):
        x = int(max(0, min(width - 1, float(parts[i]))))
        y = int(max(0, min(height - 1, float(parts[i + 1]))))
        coords.append([x, y])
    return np.array(coords, dtype=np.int32)


def _parse_direction(raw_direction: str) -> tuple[float, float] | None:
    if not raw_direction.strip():
        return None
    parts = [p.strip() for p in raw_direction.split(",") if p.strip()]
    if len(parts) != 2:
        return None
    try:
        return _safe_norm(float(parts[0]), float(parts[1]))
    except ValueError:
        return None


def _poly_contains(poly: np.ndarray, x: float, y: float) -> bool:
    if poly.size == 0:
        return False
    return cv2.pointPolygonTest(poly.astype(np.float32), (float(x), float(y)), False) >= 0


def _occupancy_ratio(detections: list[dict[str, Any]], roi_poly: np.ndarray, width: int, height: int) -> tuple[int, int, float]:
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


@dataclass
class LaneDef:
    lane_id: str
    polygon: np.ndarray


@dataclass
class Geometry:
    road_polygon: np.ndarray
    direction: tuple[float, float]
    lanes: list[LaneDef]


@dataclass
class TrackMemory:
    track_id: int
    class_name: str
    hits: int = 0
    first_seen_ts: float = 0.0
    last_seen_ts: float = 0.0
    # ts, cx, cy, projection_along_direction
    samples: deque[tuple[float, float, float, float]] = field(default_factory=deque)
    low_speed_since_ts: float | None = None


@dataclass
class StreamRuntime:
    frame_seq: int = 0
    next_local_track_id: int = 1
    tracks: dict[int, TrackMemory] = field(default_factory=dict)
    raw_to_local: dict[int, int] = field(default_factory=dict)

    def alloc_track_id(self) -> int:
        tid = self.next_local_track_id
        self.next_local_track_id += 1
        return tid


class MOTStateStore:
    def __init__(self, track_max_age_sec: float, smooth_window_sec: float, min_hits: int) -> None:
        self.track_max_age_sec = track_max_age_sec
        self.smooth_window_sec = smooth_window_sec
        self.min_hits = max(1, min_hits)
        self._lock = threading.Lock()
        self._streams: dict[str, StreamRuntime] = {}

    def update_tracks(
        self,
        stream_id: str,
        detections: list[dict[str, Any]],
        direction: tuple[float, float],
        now_ts: float,
    ) -> list[dict[str, Any]]:
        with self._lock:
            runtime = self._streams.setdefault(stream_id, StreamRuntime())
            runtime.frame_seq += 1
            self._prune_stale(runtime, now_ts)

            out: list[dict[str, Any]] = []
            seen_local_ids: set[int] = set()

            for det in detections:
                raw_track_id = det.get("_raw_track_id")
                local_track_id = None
                if isinstance(raw_track_id, int):
                    local_track_id = runtime.raw_to_local.get(raw_track_id)
                    if local_track_id is None:
                        local_track_id = runtime.alloc_track_id()
                        runtime.raw_to_local[raw_track_id] = local_track_id
                if local_track_id is None:
                    local_track_id = runtime.alloc_track_id()

                mem = runtime.tracks.get(local_track_id)
                if mem is None:
                    mem = TrackMemory(
                        track_id=local_track_id,
                        class_name=str(det.get("class_name", "vehicle")),
                        first_seen_ts=now_ts,
                        last_seen_ts=now_ts,
                    )
                    runtime.tracks[local_track_id] = mem

                mem.class_name = str(det.get("class_name", mem.class_name))
                mem.hits += 1
                mem.last_seen_ts = now_ts

                x1, y1, x2, y2 = det["bbox"]
                cx = float((x1 + x2) / 2.0)
                cy = float((y1 + y2) / 2.0)
                bcx = cx
                bcy = float(y2)
                projection = bcx * direction[0] + bcy * direction[1]
                mem.samples.append((now_ts, bcx, bcy, projection))
                self._trim_samples(mem, now_ts)

                vx, vy = self._velocity_vector(mem)
                smoothed_speed = self._smoothed_speed(mem)
                if abs(smoothed_speed) <= STOPPED_SPEED_THRESHOLD_PX_S:
                    if mem.low_speed_since_ts is None:
                        mem.low_speed_since_ts = now_ts
                else:
                    mem.low_speed_since_ts = None

                seen_local_ids.add(local_track_id)
                out.append(
                    {
                        "track_id": local_track_id,
                        "bbox": [float(x1), float(y1), float(x2), float(y2)],
                        "class_name": det["class_name"],
                        "class_id": int(det["class_id"]),
                        "confidence": float(det["confidence"]),
                        "cx": cx,
                        "cy": cy,
                        "bottom_center": [bcx, bcy],
                        "vx": vx,
                        "vy": vy,
                        "speed_px_s": abs(smoothed_speed),
                        "speed_signed_px_s": smoothed_speed,
                        "hits": mem.hits,
                        "is_mature": bool(mem.hits >= self.min_hits),
                        "stopped_for_s": float(now_ts - mem.low_speed_since_ts) if mem.low_speed_since_ts else 0.0,
                    }
                )

            # Keep only tracks seen this frame in response payload.
            return out

    def frame_seq(self, stream_id: str) -> int:
        with self._lock:
            runtime = self._streams.get(stream_id)
            if runtime is None:
                return 0
            return runtime.frame_seq

    def _trim_samples(self, mem: TrackMemory, now_ts: float) -> None:
        cutoff = now_ts - self.smooth_window_sec
        while mem.samples and mem.samples[0][0] < cutoff:
            mem.samples.popleft()

    def _velocity_vector(self, mem: TrackMemory) -> tuple[float, float]:
        if len(mem.samples) < 2:
            return (0.0, 0.0)
        t0, x0, y0, _ = mem.samples[0]
        t1, x1, y1, _ = mem.samples[-1]
        dt = max(1e-3, t1 - t0)
        return ((x1 - x0) / dt, (y1 - y0) / dt)

    def _smoothed_speed(self, mem: TrackMemory) -> float:
        if len(mem.samples) < 2:
            return 0.0
        t0, _, _, p0 = mem.samples[0]
        t1, _, _, p1 = mem.samples[-1]
        dt = max(1e-3, t1 - t0)
        return (p1 - p0) / dt

    def _prune_stale(self, runtime: StreamRuntime, now_ts: float) -> None:
        stale_local_ids = [tid for tid, mem in runtime.tracks.items() if now_ts - mem.last_seen_ts > self.track_max_age_sec]
        for tid in stale_local_ids:
            del runtime.tracks[tid]
        if stale_local_ids:
            stale_set = set(stale_local_ids)
            runtime.raw_to_local = {
                raw_id: local_id
                for raw_id, local_id in runtime.raw_to_local.items()
                if local_id not in stale_set
            }


def _load_model() -> tuple[YOLO, str]:
    model_name = os.getenv("DETECTOR_MODEL", "yolov8n.pt").strip() or "yolov8n.pt"
    return YOLO(model_name), model_name


def _load_roi_config() -> dict[str, Any]:
    path = os.getenv("DETECTOR_ROI_CONFIG_PATH", "").strip()
    if not path:
        return {}
    try:
        with open(path, "r", encoding="utf-8") as f:
            payload = json.load(f)
        if isinstance(payload, dict):
            return payload
    except Exception:
        return {}
    return {}


def _parse_polygon_points(raw_points: Any, width: int, height: int) -> np.ndarray | None:
    if not isinstance(raw_points, list) or len(raw_points) < 3:
        return None
    coords: list[list[int]] = []
    for item in raw_points:
        if not isinstance(item, list) or len(item) != 2:
            return None
        try:
            x = int(max(0, min(width - 1, float(item[0]))))
            y = int(max(0, min(height - 1, float(item[1]))))
        except (TypeError, ValueError):
            return None
        coords.append([x, y])
    return np.array(coords, dtype=np.int32)


def _parse_lanes_json(raw_lanes: str, width: int, height: int) -> list[LaneDef]:
    if not raw_lanes.strip():
        return []
    try:
        parsed = json.loads(raw_lanes)
    except json.JSONDecodeError:
        return []
    if not isinstance(parsed, list):
        return []
    out: list[LaneDef] = []
    for i, lane in enumerate(parsed):
        if not isinstance(lane, dict):
            continue
        lane_id = str(lane.get("id", f"lane_{i+1}"))
        poly = _parse_polygon_points(lane.get("polygon"), width, height)
        if poly is None:
            continue
        out.append(LaneDef(lane_id=lane_id, polygon=poly))
    return out


def _default_lane_defs(width: int, height: int, lane_count: int = 4) -> list[LaneDef]:
    """
    Build visible fallback lane polygons when no camera-specific lane config exists.
    This is an approximation for UI visualization only (not calibrated geometry).
    """
    lane_count = max(2, min(6, lane_count))
    y_top = int(height * 0.42)
    y_bottom = height - 1
    x_mid = width / 2.0
    # Narrower road width near horizon, wider near bottom to mimic perspective.
    top_half_w = width * 0.18
    bot_half_w = width * 0.45

    top_left = x_mid - top_half_w
    top_right = x_mid + top_half_w
    bot_left = x_mid - bot_half_w
    bot_right = x_mid + bot_half_w

    lanes: list[LaneDef] = []
    for i in range(lane_count):
        f0 = i / lane_count
        f1 = (i + 1) / lane_count
        p1 = [int(top_left + (top_right - top_left) * f0), y_top]
        p2 = [int(top_left + (top_right - top_left) * f1), y_top]
        p3 = [int(bot_left + (bot_right - bot_left) * f1), y_bottom]
        p4 = [int(bot_left + (bot_right - bot_left) * f0), y_bottom]
        poly = np.array([p1, p2, p3, p4], dtype=np.int32)
        lanes.append(LaneDef(lane_id=f"lane_{i+1}", polygon=poly))
    return lanes


def _resolve_geometry(
    stream_id: str,
    width: int,
    height: int,
    roi_query: str,
    direction_query: str,
    lanes_query: str,
) -> Geometry:
    cfg = ROI_CONFIG.get(stream_id) or ROI_CONFIG.get("default") or {}

    road_polygon = _parse_polygon_points(cfg.get("road_polygon"), width, height)
    if road_polygon is None:
        road_polygon = np.array(
            [[0, 0], [width - 1, 0], [width - 1, height - 1], [0, height - 1]],
            dtype=np.int32,
        )

    if roi_query.strip():
        road_polygon = _parse_roi_points(roi_query, width, height)

    direction = _safe_norm(1.0, 0.0)
    cfg_dir = cfg.get("direction")
    if isinstance(cfg_dir, list) and len(cfg_dir) == 2:
        try:
            direction = _safe_norm(float(cfg_dir[0]), float(cfg_dir[1]))
        except (TypeError, ValueError):
            pass
    parsed_direction = _parse_direction(direction_query)
    if parsed_direction is not None:
        direction = parsed_direction

    lanes: list[LaneDef] = []
    cfg_lanes = cfg.get("lanes")
    if isinstance(cfg_lanes, list):
        for i, lane in enumerate(cfg_lanes):
            if not isinstance(lane, dict):
                continue
            lane_id = str(lane.get("id", f"lane_{i+1}"))
            poly = _parse_polygon_points(lane.get("polygon"), width, height)
            if poly is None:
                continue
            lanes.append(LaneDef(lane_id=lane_id, polygon=poly))

    query_lanes = _parse_lanes_json(lanes_query, width, height)
    if query_lanes:
        lanes = query_lanes

    if not lanes:
        lanes = _default_lane_defs(width=width, height=height, lane_count=4)

    return Geometry(road_polygon=road_polygon, direction=direction, lanes=lanes)


def _resolve_lane_id(geom: Geometry, x: float, y: float) -> str | None:
    for lane in geom.lanes:
        if _poly_contains(lane.polygon, x, y):
            return lane.lane_id
    if _poly_contains(geom.road_polygon, x, y):
        return "road"
    return None


def _debug_overlay(
    image: np.ndarray,
    tracks: list[dict[str, Any]],
    geom: Geometry,
    queue_like: bool,
    stopped_like: bool,
) -> str:
    canvas = image.copy()
    cv2.polylines(canvas, [geom.road_polygon], isClosed=True, color=(0, 200, 255), thickness=2)
    for lane in geom.lanes:
        cv2.polylines(canvas, [lane.polygon], isClosed=True, color=(255, 180, 0), thickness=1)
        centroid = np.mean(lane.polygon, axis=0).astype(int)
        cv2.putText(
            canvas,
            lane.lane_id,
            (int(centroid[0]), int(centroid[1])),
            cv2.FONT_HERSHEY_SIMPLEX,
            0.45,
            (255, 180, 0),
            1,
            cv2.LINE_AA,
        )

    for t in tracks:
        x1, y1, x2, y2 = [int(v) for v in t["bbox"]]
        track_id = t["track_id"]
        lane_id = t.get("lane_id") or "-"
        speed = float(t.get("speed_px_s", 0.0))
        cv2.rectangle(canvas, (x1, y1), (x2, y2), (20, 220, 80), 2)
        label = f"id:{track_id} {lane_id} {speed:.1f}px/s"
        cv2.putText(canvas, label, (x1, max(12, y1 - 4)), cv2.FONT_HERSHEY_SIMPLEX, 0.43, (20, 220, 80), 1, cv2.LINE_AA)

    flags = f"queue_like={int(queue_like)} stopped_like={int(stopped_like)}"
    cv2.putText(canvas, flags, (10, 18), cv2.FONT_HERSHEY_SIMPLEX, 0.55, (40, 40, 255), 2, cv2.LINE_AA)

    ok, encoded = cv2.imencode(".jpg", canvas, [int(cv2.IMWRITE_JPEG_QUALITY), 82])
    if not ok:
        return ""
    return base64.b64encode(encoded.tobytes()).decode("ascii")


def _poly_to_points(poly: np.ndarray) -> list[list[int]]:
    return [[int(p[0]), int(p[1])] for p in poly.tolist()]


TRACK_MAX_AGE_SEC = _env_float("DETECTOR_TRACK_MAX_AGE_SEC", 4.0)
TRACK_MIN_HITS = _env_int("DETECTOR_TRACK_MIN_HITS", 2)
SMOOTH_WINDOW_SEC = _env_float("DETECTOR_SMOOTH_WINDOW_SEC", 1.0)
STOPPED_SPEED_THRESHOLD_PX_S = _env_float("DETECTOR_STOPPED_SPEED_THRESHOLD_PX_S", 2.0)
STOPPED_MIN_SEC = _env_float("DETECTOR_STOPPED_MIN_SEC", 20.0)
QUEUE_SPEED_THRESHOLD_PX_S = _env_float("DETECTOR_QUEUE_SPEED_THRESHOLD_PX_S", 8.0)
QUEUE_OCCUPANCY_THRESHOLD = _env_float("DETECTOR_QUEUE_OCCUPANCY_THRESHOLD", 0.18)
QUEUE_MIN_TRACKS = _env_int("DETECTOR_QUEUE_MIN_TRACKS", 4)
TRACKER_CONFIG = os.getenv("DETECTOR_TRACKER_CONFIG", "bytetrack.yaml").strip() or "bytetrack.yaml"
DEFAULT_DEBUG_OVERLAY = _env_bool("DETECTOR_DEBUG_OVERLAY_DEFAULT", False)

app = FastAPI(title="Traffic Detector Spike", version="0.2.0")
model, model_name = _load_model()
device = os.getenv("DETECTOR_DEVICE", "cpu").strip() or "cpu"
ROI_CONFIG = _load_roi_config()
mot_state = MOTStateStore(
    track_max_age_sec=TRACK_MAX_AGE_SEC,
    smooth_window_sec=max(0.25, SMOOTH_WINDOW_SEC),
    min_hits=TRACK_MIN_HITS,
)
inference_lock = threading.Lock()


@app.get("/internal/health")
def health() -> dict[str, Any]:
    return {
        "ok": True,
        "model": model_name,
        "device": device,
        "tracker": TRACKER_CONFIG,
        "track_max_age_sec": TRACK_MAX_AGE_SEC,
        "track_min_hits": TRACK_MIN_HITS,
        "smooth_window_sec": SMOOTH_WINDOW_SEC,
        "roi_config_loaded": bool(ROI_CONFIG),
    }


@app.post("/internal/detect")
async def detect(
    request: Request,
    stream_id: str = Query("default"),
    conf: float = Query(0.25, ge=0.01, le=0.95),
    iou: float = Query(0.45, ge=0.05, le=0.95),
    imgsz: int = Query(640, ge=160, le=1280),
    roi: str = Query(default=""),
    lanes: str = Query(default=""),
    direction: str = Query(default=""),
    moving_speed_threshold_px_s: float = Query(12.0, ge=0.1, le=2000.0),
    smoothing_window_sec: float = Query(default=0.0),
    debug_overlay: bool = Query(default=DEFAULT_DEBUG_OVERLAY),
) -> dict[str, Any]:
    image_bytes = await request.body()
    if not image_bytes:
        raise HTTPException(status_code=400, detail="request body must contain JPEG/PNG bytes")

    np_bytes = np.frombuffer(image_bytes, dtype=np.uint8)
    image = cv2.imdecode(np_bytes, cv2.IMREAD_COLOR)
    if image is None:
        raise HTTPException(status_code=400, detail="failed to decode image bytes")

    height, width = image.shape[:2]
    geom = _resolve_geometry(stream_id=stream_id, width=width, height=height, roi_query=roi, direction_query=direction, lanes_query=lanes)

    t0 = time.perf_counter()
    with inference_lock:
        results = model.track(
            source=image,
            conf=conf,
            iou=iou,
            imgsz=imgsz,
            device=device,
            tracker=TRACKER_CONFIG,
            persist=True,
            verbose=False,
        )
    inference_ms = (time.perf_counter() - t0) * 1000.0
    now_ts = time.time()
    frame_ts_unix_ms = int(now_ts * 1000)

    names = results[0].names
    parsed_detections: list[dict[str, Any]] = []
    boxes = results[0].boxes
    if boxes is not None:
        track_ids = boxes.id.tolist() if boxes.id is not None else [None] * len(boxes)
        cls_vals = boxes.cls.tolist() if boxes.cls is not None else []
        conf_vals = boxes.conf.tolist() if boxes.conf is not None else []
        xyxy_vals = boxes.xyxy.tolist() if boxes.xyxy is not None else []

        for idx in range(min(len(xyxy_vals), len(cls_vals), len(conf_vals), len(track_ids))):
            cls_id = int(cls_vals[idx])
            class_name = str(names.get(cls_id, cls_id))
            if class_name not in TARGET_CLASSES:
                continue
            x1, y1, x2, y2 = [float(v) for v in xyxy_vals[idx]]
            raw_track_id = track_ids[idx]
            parsed_detections.append(
                {
                    "bbox": [x1, y1, x2, y2],
                    "class_id": cls_id,
                    "class_name": class_name,
                    "confidence": float(conf_vals[idx]),
                    "_raw_track_id": int(raw_track_id) if raw_track_id is not None else None,
                }
            )

    if smoothing_window_sec > 0:
        mot_state.smooth_window_sec = max(0.25, min(2.0, smoothing_window_sec))

    tracks = mot_state.update_tracks(stream_id=stream_id, detections=parsed_detections, direction=geom.direction, now_ts=now_ts)
    frame_seq = mot_state.frame_seq(stream_id)

    detections: list[dict[str, Any]] = []
    mature_roi_tracks: list[dict[str, Any]] = []
    tracks_out: list[dict[str, Any]] = []
    lane_counts: dict[str, int] = {}
    moving_count = 0
    stopped_like = False

    for t in tracks:
        cx = float(t["cx"])
        cy = float(t["cy"])
        bcx, bcy = t["bottom_center"]
        lane_id = _resolve_lane_id(geom, float(bcx), float(bcy))
        in_roi = bool(lane_id is not None and lane_id != "")
        if in_roi:
            lane_counts[lane_id] = lane_counts.get(lane_id, 0) + 1
        speed_px_s = abs(float(t["speed_px_s"]))
        is_moving = speed_px_s >= moving_speed_threshold_px_s
        if is_moving:
            moving_count += 1

        stopped_for = float(t.get("stopped_for_s", 0.0))
        if in_roi and stopped_for >= STOPPED_MIN_SEC:
            stopped_like = True

        track_payload = {
            "track_id": t["track_id"],
            "bbox": t["bbox"],
            "class_name": t["class_name"],
            "confidence": t["confidence"],
            "cx": cx,
            "cy": cy,
            "vx": float(t["vx"]),
            "vy": float(t["vy"]),
            "speed_px_s": speed_px_s,
            "speed_signed_px_s": float(t["speed_signed_px_s"]),
            "lane_id": lane_id,
            "in_roi": in_roi,
            "hits": int(t["hits"]),
            "is_mature": bool(t["is_mature"]),
            "stopped_for_s": stopped_for,
        }
        tracks_out.append(track_payload)

        detections.append(
            {
                "bbox": t["bbox"],
                "class_id": t["class_id"],
                "class_name": t["class_name"],
                "confidence": t["confidence"],
                "center": [cx, cy],
                "cx": cx,
                "cy": cy,
                "track_id": t["track_id"],
                "speed_px_s": speed_px_s,
                "is_moving": is_moving,
                "lane_id": lane_id,
                "smoothed_speed_px_s": speed_px_s,
            }
        )

        if in_roi and track_payload["is_mature"]:
            mature_roi_tracks.append(track_payload)

    speed_values = [float(t["speed_px_s"]) for t in mature_roi_tracks]
    mean_speed = float(np.mean(speed_values)) if speed_values else 0.0
    median_speed = float(np.median(speed_values)) if speed_values else 0.0

    occupied_px, roi_area_px, occupancy_ratio = _occupancy_ratio(
        detections=detections,
        roi_poly=geom.road_polygon,
        width=width,
        height=height,
    )

    queue_like = (
        len(mature_roi_tracks) >= max(1, QUEUE_MIN_TRACKS)
        and occupancy_ratio >= QUEUE_OCCUPANCY_THRESHOLD
        and median_speed <= QUEUE_SPEED_THRESHOLD_PX_S
    )

    total_tracks = len(tracks)
    stationary_count = max(0, total_tracks - moving_count)
    moving_ratio = float(moving_count / total_tracks) if total_tracks > 0 else 0.0

    payload: dict[str, Any] = {
        "model": model_name,
        "device": device,
        "tracker": TRACKER_CONFIG,
        "inference_ms": round(inference_ms, 2),
        "image": {"width": width, "height": height},
        "geometry": {
            "road_polygon": _poly_to_points(geom.road_polygon),
            "lanes": [{"lane_id": lane.lane_id, "polygon": _poly_to_points(lane.polygon)} for lane in geom.lanes],
            "direction": [float(geom.direction[0]), float(geom.direction[1])],
        },
        "stream_id": stream_id,
        "frame_seq": frame_seq,
        "frame_ts_unix_ms": frame_ts_unix_ms,
        "ts_unix_ms": frame_ts_unix_ms,
        # Backward compatible payload used by current UI.
        "detections": detections,
        # New richer tracking contract for downstream analytics (always geometry-enriched).
        "tracks": [
            {
                "track_id": t["track_id"],
                "bbox": t["bbox"],
                "class_name": t["class_name"],
                "confidence": t["confidence"],
                "cx": t["cx"],
                "cy": t["cy"],
                "vx": t["vx"],
                "vy": t["vy"],
                "lane_id": t["lane_id"],
                "speed_px_s": t["speed_px_s"],
                "in_roi": t["in_roi"],
                "stopped_for_s": t["stopped_for_s"],
            }
            for t in tracks_out
        ],
        "metrics": {
            "occupancy_ratio": occupancy_ratio,
            "occupied_px": occupied_px,
            "roi_area_px": roi_area_px,
            "vehicle_count": total_tracks,
            "moving_vehicle_count": moving_count,
            "stationary_vehicle_count": stationary_count,
            "moving_ratio": moving_ratio,
            "mean_track_speed_px_s": mean_speed,
            "counts_per_lane": lane_counts,
            "counts_per_roi": {"road": sum(lane_counts.values())},
            "mean_smoothed_speed_px_s": mean_speed,
            "median_smoothed_speed_px_s": median_speed,
            "queue_like": bool(queue_like),
            "stopped_like": bool(stopped_like),
            "speed_units": "px/s",
        },
        "metric_definitions": {
            "occupancy_ratio": "fraction of ROI pixels covered by union of vehicle detection boxes",
            "moving_ratio": "moving vehicles / total vehicles from MOT outputs",
            "mean_track_speed_px_s": "mean smoothed speed proxy in pixels/sec along road direction",
            "queue_like": "heuristic: high occupancy + low median speed + enough tracked vehicles in ROI",
            "stopped_like": "heuristic: at least one mature ROI track below stopped speed threshold for minimum duration",
        },
    }

    if debug_overlay:
        payload["debug_overlay_jpeg_b64"] = _debug_overlay(
            image=image,
            tracks=(mature_roi_tracks if mature_roi_tracks else tracks),
            geom=geom,
            queue_like=bool(queue_like),
            stopped_like=bool(stopped_like),
        )
    return payload
