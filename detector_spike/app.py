from __future__ import annotations

import base64
import json
import logging
import os
import threading
import time
from collections import deque
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Optional

import cv2
import numpy as np
from fastapi import FastAPI, HTTPException, Query, Request
from ultralytics import YOLO

TARGET_CLASSES = {"car", "truck", "bus", "motorcycle"}
LOGGER = logging.getLogger("detector_spike")


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
    priority: int = 0


@dataclass
class Geometry:
    road_polygon: np.ndarray
    direction: tuple[float, float]
    lanes: list[LaneDef]
    lane_assignment_active: bool = False
    lane_assignment_reason: str = "missing_config"
    coordinate_space: str = "decoded_frame_pixels"
    detector_input_resolution: tuple[int, int] | None = None
    homography: list[list[float]] | None = None


@dataclass
class LaneGeometryStore:
    schema_version: str = "lane-geometry-v1"
    coordinate_space: str = "decoded_frame_pixels"
    lane_assignment_anchor: str = "bbox_bottom_center"
    overlap_tiebreak: str = "config_order"
    lane_id_unknown_label: str = "unknown"
    detector_input_resolution: tuple[int, int] | None = None
    stream_specs: dict[str, "StreamGeometrySpec"] = field(default_factory=dict)
    stream_errors: dict[str, str] = field(default_factory=dict)
    source: str = "none"
    error: str = ""


@dataclass
class StreamGeometrySpec:
    stream_key: str
    lanes: list[LaneDef]
    source: str
    input_resolution: tuple[int, int] | None = None
    road_polygon: np.ndarray | None = None
    direction: tuple[float, float] | None = None
    homography: list[list[float]] | None = None


@dataclass
class TrackMemory:
    track_id: int
    class_name: str
    hits: int = 0
    first_seen_ts: float = 0.0
    last_seen_ts: float = 0.0
    last_bbox: tuple[float, float, float, float] | None = None
    smoothed_bbox: tuple[float, float, float, float] | None = None
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
    def __init__(
        self,
        track_max_age_sec: float,
        smooth_window_sec: float,
        min_hits: int,
        assoc_iou_threshold: float,
        bbox_ema_alpha: float,
        assoc_center_max_px: float = 0.0,
    ) -> None:
        self.track_max_age_sec = track_max_age_sec
        self.smooth_window_sec = smooth_window_sec
        self.min_hits = max(1, min_hits)
        self.assoc_iou_threshold = max(0.0, min(0.95, assoc_iou_threshold))
        self.bbox_ema_alpha = max(0.0, min(1.0, bbox_ema_alpha))
        self.assoc_center_max_px = max(0.0, assoc_center_max_px)
        self._lock = threading.Lock()
        self._streams: dict[str, StreamRuntime] = {}

    def update_tracks(
        self,
        stream_id: str,
        detections: list[dict[str, Any]],
        direction: tuple[float, float],
        now_ts: float,
        assoc_iou_threshold: Optional[float] = None,
        assoc_center_max_px: Optional[float] = None,
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
                    local_track_id = self._associate_by_iou(
                        runtime=runtime,
                        bbox=det["bbox"],
                        seen_local_ids=seen_local_ids,
                        now_ts=now_ts,
                        assoc_iou_threshold=assoc_iou_threshold,
                        assoc_center_max_px=assoc_center_max_px,
                    )
                if local_track_id is None:
                    local_track_id = runtime.alloc_track_id()
                if isinstance(raw_track_id, int):
                    runtime.raw_to_local[raw_track_id] = local_track_id

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
                mem.last_bbox = (float(x1), float(y1), float(x2), float(y2))
                if mem.smoothed_bbox is None:
                    mem.smoothed_bbox = mem.last_bbox
                else:
                    mem.smoothed_bbox = self._smooth_bbox(
                        prev=mem.smoothed_bbox,
                        current=mem.last_bbox,
                    )
                sx1, sy1, sx2, sy2 = mem.smoothed_bbox
                cx = float((sx1 + sx2) / 2.0)
                cy = float((sy1 + sy2) / 2.0)
                bcx = cx
                bcy = float(sy2)
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
                        "bbox": [float(sx1), float(sy1), float(sx2), float(sy2)],
                        "bbox_raw": [float(x1), float(y1), float(x2), float(y2)],
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

    def _smooth_bbox(
        self,
        prev: tuple[float, float, float, float],
        current: tuple[float, float, float, float],
    ) -> tuple[float, float, float, float]:
        a = self.bbox_ema_alpha
        if a <= 0.0:
            return current
        if a >= 1.0:
            return prev
        px1, py1, px2, py2 = prev
        cx1, cy1, cx2, cy2 = current
        return (
            (1.0 - a) * cx1 + a * px1,
            (1.0 - a) * cy1 + a * py1,
            (1.0 - a) * cx2 + a * px2,
            (1.0 - a) * cy2 + a * py2,
        )

    @staticmethod
    def _mem_bbox_for_assoc(mem: TrackMemory) -> Optional[tuple[float, float, float, float]]:
        if mem.smoothed_bbox is not None:
            return mem.smoothed_bbox
        return mem.last_bbox

    @staticmethod
    def _center_distance_px(
        a: tuple[float, float, float, float],
        b: list[float],
    ) -> float:
        ax1, ay1, ax2, ay2 = a
        bx1, by1, bx2, by2 = [float(v) for v in b]
        acx = (ax1 + ax2) * 0.5
        acy = (ay1 + ay2) * 0.5
        bcx = (bx1 + bx2) * 0.5
        bcy = (by1 + by2) * 0.5
        dx = acx - bcx
        dy = acy - bcy
        return float((dx * dx + dy * dy) ** 0.5)

    def _associate_by_iou(
        self,
        runtime: StreamRuntime,
        bbox: list[float],
        seen_local_ids: set[int],
        now_ts: float,
        assoc_iou_threshold: Optional[float] = None,
        assoc_center_max_px: Optional[float] = None,
    ) -> Optional[int]:
        iou_thr = self.assoc_iou_threshold if assoc_iou_threshold is None else max(0.0, min(0.95, float(assoc_iou_threshold)))
        center_max = (
            self.assoc_center_max_px if assoc_center_max_px is None else max(0.0, float(assoc_center_max_px))
        )
        best_local_id: int | None = None
        best_iou = 0.0
        best_center_id: int | None = None
        best_center_dist = float("inf")
        for local_id, mem in runtime.tracks.items():
            if local_id in seen_local_ids:
                continue
            if now_ts - mem.last_seen_ts > self.track_max_age_sec:
                continue
            prior = self._mem_bbox_for_assoc(mem)
            if prior is None:
                continue
            iou = self._bbox_iou(prior, bbox)
            if iou > best_iou:
                best_iou = iou
                best_local_id = local_id
            if center_max > 0:
                dist = self._center_distance_px(prior, bbox)
                if dist <= center_max and dist < best_center_dist:
                    best_center_dist = dist
                    best_center_id = local_id
        if best_local_id is not None and best_iou >= iou_thr:
            return best_local_id
        if best_center_id is not None:
            return best_center_id
        return None

    def _bbox_iou(self, a: tuple[float, float, float, float], b: list[float]) -> float:
        ax1, ay1, ax2, ay2 = a
        bx1, by1, bx2, by2 = [float(v) for v in b]
        ix1 = max(ax1, bx1)
        iy1 = max(ay1, by1)
        ix2 = min(ax2, bx2)
        iy2 = min(ay2, by2)
        iw = max(0.0, ix2 - ix1)
        ih = max(0.0, iy2 - iy1)
        inter = iw * ih
        if inter <= 0:
            return 0.0
        area_a = max(0.0, ax2 - ax1) * max(0.0, ay2 - ay1)
        area_b = max(0.0, bx2 - bx1) * max(0.0, by2 - by1)
        denom = area_a + area_b - inter
        if denom <= 1e-6:
            return 0.0
        return inter / denom

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


def _parse_resolution(raw: Any) -> tuple[int, int] | None:
    if not isinstance(raw, dict):
        return None
    try:
        w = int(raw.get("width"))
        h = int(raw.get("height"))
    except (TypeError, ValueError):
        return None
    if w <= 0 or h <= 0:
        return None
    return (w, h)


def _poly_area(poly: np.ndarray) -> float:
    if poly.size == 0 or len(poly) < 3:
        return 0.0
    x = poly[:, 0].astype(np.float64)
    y = poly[:, 1].astype(np.float64)
    return float(abs(np.dot(x, np.roll(y, -1)) - np.dot(y, np.roll(x, -1))) / 2.0)


def _scale_polygon(poly: np.ndarray, src_size: tuple[int, int], dst_size: tuple[int, int]) -> np.ndarray:
    src_w, src_h = src_size
    dst_w, dst_h = dst_size
    if src_w <= 0 or src_h <= 0 or dst_w <= 0 or dst_h <= 0:
        return poly.astype(np.int32)
    sx = float(dst_w) / float(src_w)
    sy = float(dst_h) / float(src_h)
    scaled = poly.astype(np.float32).copy()
    scaled[:, 0] = scaled[:, 0] * sx
    scaled[:, 1] = scaled[:, 1] * sy
    scaled[:, 0] = np.clip(scaled[:, 0], 0, max(0, dst_w - 1))
    scaled[:, 1] = np.clip(scaled[:, 1], 0, max(0, dst_h - 1))
    return np.round(scaled).astype(np.int32)


def _load_roi_config() -> LaneGeometryStore:
    path = os.getenv("DETECTOR_ROI_CONFIG_PATH", "").strip()
    if not path:
        return LaneGeometryStore(source="none", error="missing_config_path")
    path_obj = Path(path).expanduser()
    if not path_obj.exists():
        return LaneGeometryStore(source=str(path_obj), error="config_path_not_found")
    if not path_obj.is_file():
        return LaneGeometryStore(source=str(path_obj), error="config_path_not_file")
    try:
        with path_obj.open("r", encoding="utf-8") as f:
            payload = json.load(f)
    except Exception as exc:
        return LaneGeometryStore(source=str(path_obj), error=f"config_read_error: {exc}")

    if not isinstance(payload, dict):
        return LaneGeometryStore(source=str(path_obj), error="invalid_root_object")
    schema = str(payload.get("schema_version", "")).strip()
    if schema != "lane-geometry-v1":
        return LaneGeometryStore(source=str(path_obj), error=f"unsupported_schema_version:{schema or 'empty'}")

    coordinate_space = str(payload.get("coordinate_space", "decoded_frame_pixels")).strip() or "decoded_frame_pixels"
    lane_assignment_anchor = str(payload.get("lane_assignment_anchor", "bbox_bottom_center")).strip() or "bbox_bottom_center"
    overlap_tiebreak = str(payload.get("overlap_tiebreak", "config_order")).strip() or "config_order"
    unknown_label = str(payload.get("lane_id_unknown_label", "unknown")).strip() or "unknown"
    default_resolution = _parse_resolution(payload.get("detector_input_resolution"))
    streams_raw = payload.get("streams")
    if not isinstance(streams_raw, dict):
        return LaneGeometryStore(source=str(path_obj), error="streams_missing_or_invalid")

    store = LaneGeometryStore(
        schema_version=schema,
        coordinate_space=coordinate_space,
        lane_assignment_anchor=lane_assignment_anchor,
        overlap_tiebreak=overlap_tiebreak,
        lane_id_unknown_label=unknown_label,
        detector_input_resolution=default_resolution,
        stream_specs={},
        stream_errors={},
        source=str(path_obj),
        error="",
    )
    for stream_key, spec_raw in streams_raw.items():
        stream_id = str(stream_key).strip()
        if not stream_id:
            continue
        if not isinstance(spec_raw, dict):
            store.stream_errors[stream_id] = "stream_spec_not_object"
            continue

        spec_res = _parse_resolution(spec_raw.get("detector_input_resolution")) or default_resolution
        parse_w = spec_res[0] if spec_res else 8192
        parse_h = spec_res[1] if spec_res else 8192
        lanes_raw = spec_raw.get("lanes")
        if not isinstance(lanes_raw, list) or len(lanes_raw) == 0:
            store.stream_errors[stream_id] = "lanes_missing_or_empty"
            continue

        lanes: list[LaneDef] = []
        lane_valid = True
        for idx, lane_raw in enumerate(lanes_raw):
            if not isinstance(lane_raw, dict):
                lane_valid = False
                break
            lane_id = str(lane_raw.get("lane_id", "")).strip()
            if not lane_id:
                lane_valid = False
                break
            lane_poly = _parse_polygon_points(lane_raw.get("polygon"), parse_w, parse_h)
            if lane_poly is None:
                lane_valid = False
                break
            try:
                priority = int(lane_raw.get("priority", idx))
            except (TypeError, ValueError):
                lane_valid = False
                break
            lanes.append(LaneDef(lane_id=lane_id, polygon=lane_poly, priority=priority))

        if not lane_valid or not lanes:
            store.stream_errors[stream_id] = "invalid_lane_geometry"
            continue

        direction_val: tuple[float, float] | None = None
        raw_direction = spec_raw.get("direction")
        if isinstance(raw_direction, list) and len(raw_direction) == 2:
            try:
                direction_val = _safe_norm(float(raw_direction[0]), float(raw_direction[1]))
            except (TypeError, ValueError):
                direction_val = None

        road_poly = _parse_polygon_points(spec_raw.get("road_polygon"), parse_w, parse_h)
        homography = spec_raw.get("homography")
        if homography is not None:
            if (
                not isinstance(homography, list)
                or len(homography) != 3
                or any((not isinstance(row, list)) or len(row) != 3 for row in homography)
            ):
                homography = None

        store.stream_specs[stream_id] = StreamGeometrySpec(
            stream_key=stream_id,
            lanes=lanes,
            source=str(path_obj),
            input_resolution=spec_res,
            road_polygon=road_poly,
            direction=direction_val,
            homography=homography if isinstance(homography, list) else None,
        )
    if store.error:
        LOGGER.warning("lane geometry config invalid: source=%s error=%s", store.source, store.error)
    elif store.stream_errors:
        LOGGER.warning(
            "lane geometry config loaded with stream errors: source=%s bad_streams=%s",
            store.source,
            sorted(store.stream_errors.keys()),
        )
    else:
        LOGGER.info("lane geometry config loaded: source=%s streams=%d", store.source, len(store.stream_specs))
    return store


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
        lane_id = str(lane.get("lane_id", lane.get("id", f"lane_{i+1}"))).strip()
        if not lane_id:
            continue
        poly = _parse_polygon_points(lane.get("polygon"), width, height)
        if poly is None:
            continue
        try:
            priority = int(lane.get("priority", i))
        except (TypeError, ValueError):
            priority = i
        out.append(LaneDef(lane_id=lane_id, polygon=poly, priority=priority))
    return out


def _resolve_geometry(
    stream_id: str,
    width: int,
    height: int,
    roi_query: str,
    direction_query: str,
    lanes_query: str,
) -> Geometry:
    full_frame = np.array(
        [[0, 0], [width - 1, 0], [width - 1, height - 1], [0, height - 1]],
        dtype=np.int32,
    )
    direction = _safe_norm(1.0, 0.0)
    parsed_direction = _parse_direction(direction_query)
    if parsed_direction is not None:
        direction = parsed_direction

    query_lanes = _parse_lanes_json(lanes_query, width, height)
    if query_lanes:
        road_polygon = _parse_roi_points(roi_query, width, height) if roi_query.strip() else full_frame
        return Geometry(
            road_polygon=road_polygon,
            direction=direction,
            lanes=query_lanes,
            lane_assignment_active=True,
            lane_assignment_reason="query_override",
            coordinate_space="decoded_frame_pixels",
            detector_input_resolution=(width, height),
            homography=None,
        )

    spec = ROI_CONFIG.stream_specs.get(stream_id)
    if spec is None:
        reason = ROI_CONFIG.stream_errors.get(stream_id, "missing_stream_geometry")
        road_polygon = _parse_roi_points(roi_query, width, height) if roi_query.strip() else full_frame
        return Geometry(
            road_polygon=road_polygon,
            direction=direction,
            lanes=[],
            lane_assignment_active=False,
            lane_assignment_reason=reason,
            coordinate_space=ROI_CONFIG.coordinate_space,
            detector_input_resolution=(width, height),
            homography=None,
        )

    spec_dir = spec.direction if spec.direction is not None else direction
    direction = parsed_direction if parsed_direction is not None else spec_dir
    src_size = spec.input_resolution if spec.input_resolution else (width, height)
    dst_size = (width, height)
    scaled_lanes = [
        LaneDef(
            lane_id=lane.lane_id,
            polygon=_scale_polygon(lane.polygon, src_size=src_size, dst_size=dst_size)
            if src_size != dst_size
            else lane.polygon.copy(),
            priority=lane.priority,
        )
        for lane in spec.lanes
    ]
    if ROI_CONFIG.overlap_tiebreak == "largest_area":
        scaled_lanes.sort(key=lambda lane: _poly_area(lane.polygon), reverse=True)
    else:
        scaled_lanes.sort(key=lambda lane: lane.priority)

    if roi_query.strip():
        road_polygon = _parse_roi_points(roi_query, width, height)
    elif spec.road_polygon is not None:
        road_polygon = (
            _scale_polygon(spec.road_polygon, src_size=src_size, dst_size=dst_size)
            if src_size != dst_size
            else spec.road_polygon.copy()
        )
    else:
        road_polygon = full_frame

    return Geometry(
        road_polygon=road_polygon,
        direction=direction,
        lanes=scaled_lanes,
        lane_assignment_active=True,
        lane_assignment_reason="ok",
        coordinate_space=ROI_CONFIG.coordinate_space,
        detector_input_resolution=dst_size,
        homography=spec.homography,
    )


def _resolve_assignment_point(
    geom: Geometry,
    bbox: list[float],
    cx: float,
    cy: float,
    bottom_center: list[float],
) -> tuple[float, float]:
    anchor = ROI_CONFIG.lane_assignment_anchor
    if anchor == "bbox_center":
        return (float(cx), float(cy))
    if anchor == "bbox_footprint_center":
        x1, _, x2, y2 = bbox
        return (float((x1 + x2) / 2.0), float(y2))
    # default / bbox_bottom_center
    return (float(bottom_center[0]), float(bottom_center[1]))


def _lane_assignment_status(reason: str, active: bool) -> str:
    if active:
        return "ok"
    lowered = reason.lower()
    if "missing" in lowered or "not_found" in lowered:
        return "missing"
    if "invalid" in lowered or "unsupported" in lowered or "error" in lowered:
        return "invalid"
    return "disabled"


def _resolve_lane_id(geom: Geometry, x: float, y: float) -> str | None:
    if not geom.lane_assignment_active or not geom.lanes:
        return None
    for lane in geom.lanes:
        if _poly_contains(lane.polygon, x, y):
            return lane.lane_id
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


def _bgr_rec601_luma_gray(image: np.ndarray) -> np.ndarray:
    """Per-pixel luma (Rec.601) for BGR uint8 images."""
    b = image[:, :, 0].astype(np.float32)
    g = image[:, :, 1].astype(np.float32)
    r = image[:, :, 2].astype(np.float32)
    return r * 0.299 + g * 0.587 + b * 0.114


def _apply_night_enhancement(image: np.ndarray) -> tuple[np.ndarray, bool, dict[str, float]]:
    """CLAHE on L* channel for dark scenes. Gates on median luma so headlight bloom does not skip enhancement."""
    meta: dict[str, float] = {"luma_mean": 0.0, "luma_median": 0.0}
    if image.size == 0:
        return image, False, meta
    try:
        gray = _bgr_rec601_luma_gray(image)
        meta["luma_mean"] = float(np.mean(gray))
        meta["luma_median"] = float(np.median(gray))
    except Exception:
        return image, False, meta
    if not NIGHT_ENHANCE_ENABLED:
        return image, False, meta
    if meta["luma_median"] >= NIGHT_LUMA_MEDIAN_THRESHOLD:
        return image, False, meta
    try:
        lab = cv2.cvtColor(image, cv2.COLOR_BGR2LAB)
        l_chan, a_chan, b_chan = cv2.split(lab)
        tile = max(2, NIGHT_CLAHE_TILE_SIZE)
        clahe = cv2.createCLAHE(clipLimit=max(1.0, NIGHT_CLAHE_CLIP_LIMIT), tileGridSize=(tile, tile))
        l_eq = clahe.apply(l_chan)
        merged = cv2.merge((l_eq, a_chan, b_chan))
        out = cv2.cvtColor(merged, cv2.COLOR_LAB2BGR)
        return out, True, meta
    except Exception:
        return image, False, meta


def _night_luma_median_threshold() -> float:
    med = os.getenv("DETECTOR_NIGHT_LUMA_MEDIAN_THRESHOLD", "").strip()
    if med:
        try:
            return float(med)
        except ValueError:
            pass
    return _env_float("DETECTOR_NIGHT_LUMA_THRESHOLD", 105.0)


TRACK_MAX_AGE_SEC = _env_float("DETECTOR_TRACK_MAX_AGE_SEC", 4.0)
TRACK_MIN_HITS = _env_int("DETECTOR_TRACK_MIN_HITS", 2)
SMOOTH_WINDOW_SEC = _env_float("DETECTOR_SMOOTH_WINDOW_SEC", 1.0)
STOPPED_SPEED_THRESHOLD_PX_S = _env_float("DETECTOR_STOPPED_SPEED_THRESHOLD_PX_S", 2.0)
STOPPED_MIN_SEC = _env_float("DETECTOR_STOPPED_MIN_SEC", 20.0)
QUEUE_SPEED_THRESHOLD_PX_S = _env_float("DETECTOR_QUEUE_SPEED_THRESHOLD_PX_S", 8.0)
QUEUE_OCCUPANCY_THRESHOLD = _env_float("DETECTOR_QUEUE_OCCUPANCY_THRESHOLD", 0.18)
QUEUE_MIN_TRACKS = _env_int("DETECTOR_QUEUE_MIN_TRACKS", 4)
TRACKER_CONFIG = os.getenv("DETECTOR_TRACKER_CONFIG", "bytetrack.yaml").strip() or "bytetrack.yaml"
TRACK_ASSOC_IOU_THRESHOLD = _env_float("DETECTOR_TRACK_ASSOC_IOU_THRESHOLD", 0.25)
# When >0: if IoU is below threshold (common at low detect FPS), still match the prior track
# whose smoothed bbox center is nearest within this many pixels. Set 0 to disable.
TRACK_ASSOC_CENTER_MAX_PX = _env_float("DETECTOR_TRACK_ASSOC_CENTER_MAX_PX", 96.0)
BBOX_EMA_ALPHA = _env_float("DETECTOR_BBOX_EMA_ALPHA", 0.45)
NIGHT_ENHANCE_ENABLED = _env_bool("DETECTOR_NIGHT_ENHANCE_ENABLED", True)
NIGHT_LUMA_MEDIAN_THRESHOLD = _night_luma_median_threshold()
NIGHT_CLAHE_CLIP_LIMIT = _env_float("DETECTOR_NIGHT_CLAHE_CLIP_LIMIT", 2.0)
NIGHT_CLAHE_TILE_SIZE = _env_int("DETECTOR_NIGHT_CLAHE_TILE_SIZE", 8)
DEFAULT_DEBUG_OVERLAY = _env_bool("DETECTOR_DEBUG_OVERLAY_DEFAULT", False)

app = FastAPI(title="Traffic Detector Spike", version="0.2.0")
model, model_name = _load_model()
device = os.getenv("DETECTOR_DEVICE", "cpu").strip() or "cpu"
ROI_CONFIG = _load_roi_config()
mot_state = MOTStateStore(
    track_max_age_sec=TRACK_MAX_AGE_SEC,
    smooth_window_sec=max(0.25, SMOOTH_WINDOW_SEC),
    min_hits=TRACK_MIN_HITS,
    assoc_iou_threshold=TRACK_ASSOC_IOU_THRESHOLD,
    bbox_ema_alpha=BBOX_EMA_ALPHA,
    assoc_center_max_px=TRACK_ASSOC_CENTER_MAX_PX,
)
inference_lock = threading.Lock()


@app.get("/internal/health")
def health() -> dict[str, Any]:
    loaded_ok = ROI_CONFIG.error == "" and len(ROI_CONFIG.stream_specs) > 0
    return {
        "ok": True,
        "model": model_name,
        "device": device,
        "tracker": TRACKER_CONFIG,
        "track_max_age_sec": TRACK_MAX_AGE_SEC,
        "track_min_hits": TRACK_MIN_HITS,
        "track_assoc_iou_threshold": TRACK_ASSOC_IOU_THRESHOLD,
        "track_assoc_center_max_px": TRACK_ASSOC_CENTER_MAX_PX,
        "track_bbox_ema_alpha": BBOX_EMA_ALPHA,
        "smooth_window_sec": SMOOTH_WINDOW_SEC,
        "night_enhance_enabled": NIGHT_ENHANCE_ENABLED,
        "night_luma_median_threshold": NIGHT_LUMA_MEDIAN_THRESHOLD,
        "night_clahe_clip_limit": NIGHT_CLAHE_CLIP_LIMIT,
        "night_clahe_tile_size": NIGHT_CLAHE_TILE_SIZE,
        "roi_config_loaded": loaded_ok,
        "lane_geometry_source": ROI_CONFIG.source,
        "lane_geometry_schema_version": ROI_CONFIG.schema_version,
        "lane_geometry_error": ROI_CONFIG.error or None,
        "lane_geometry_stream_count": len(ROI_CONFIG.stream_specs),
        "lane_geometry_stream_errors": ROI_CONFIG.stream_errors,
        "lane_assignment_anchor": ROI_CONFIG.lane_assignment_anchor,
        "lane_overlap_tiebreak": ROI_CONFIG.overlap_tiebreak,
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
    enhanced_preview: bool = Query(default=False),
    track_assoc_iou_threshold: Optional[float] = Query(default=None, ge=0.05, le=0.95),
    track_assoc_center_max_px: Optional[float] = Query(default=None, ge=0.0, le=400.0),
) -> dict[str, Any]:
    image_bytes = await request.body()
    if not image_bytes:
        raise HTTPException(status_code=400, detail="request body must contain JPEG/PNG bytes")

    np_bytes = np.frombuffer(image_bytes, dtype=np.uint8)
    image = cv2.imdecode(np_bytes, cv2.IMREAD_COLOR)
    if image is None:
        raise HTTPException(status_code=400, detail="failed to decode image bytes")
    inference_image, night_applied, night_luma_meta = _apply_night_enhancement(image)

    height, width = image.shape[:2]
    geom = _resolve_geometry(stream_id=stream_id, width=width, height=height, roi_query=roi, direction_query=direction, lanes_query=lanes)

    t0 = time.perf_counter()
    with inference_lock:
        results = model.track(
            source=inference_image,
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

    tracks = mot_state.update_tracks(
        stream_id=stream_id,
        detections=parsed_detections,
        direction=geom.direction,
        now_ts=now_ts,
        assoc_iou_threshold=track_assoc_iou_threshold,
        assoc_center_max_px=track_assoc_center_max_px,
    )
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
        assignment_x, assignment_y = _resolve_assignment_point(
            geom=geom,
            bbox=t["bbox"],
            cx=cx,
            cy=cy,
            bottom_center=t["bottom_center"],
        )
        lane_id = _resolve_lane_id(geom, float(assignment_x), float(assignment_y))
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
            "lane_anchor_point": [float(assignment_x), float(assignment_y)],
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
                "lane_anchor_point": [float(assignment_x), float(assignment_y)],
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
    lane_status = _lane_assignment_status(geom.lane_assignment_reason, geom.lane_assignment_active)

    iou_assoc_eff = (
        float(track_assoc_iou_threshold) if track_assoc_iou_threshold is not None else TRACK_ASSOC_IOU_THRESHOLD
    )
    center_eff = (
        float(track_assoc_center_max_px) if track_assoc_center_max_px is not None else TRACK_ASSOC_CENTER_MAX_PX
    )

    payload: dict[str, Any] = {
        "model": model_name,
        "device": device,
        "tracker": TRACKER_CONFIG,
        "inference_ms": round(inference_ms, 2),
        "detector_tuning": {
            "conf": conf,
            "iou": iou,
            "imgsz": imgsz,
            "track_assoc_iou_threshold": round(iou_assoc_eff, 4),
            "track_assoc_center_max_px": round(center_eff, 2),
        },
        "image": {"width": width, "height": height},
        "geometry": {
            "road_polygon": _poly_to_points(geom.road_polygon),
            "lanes": [{"lane_id": lane.lane_id, "polygon": _poly_to_points(lane.polygon)} for lane in geom.lanes],
            "direction": [float(geom.direction[0]), float(geom.direction[1])],
            "coordinate_space": geom.coordinate_space,
            "detector_input_resolution": (
                {"width": int(geom.detector_input_resolution[0]), "height": int(geom.detector_input_resolution[1])}
                if geom.detector_input_resolution is not None
                else None
            ),
            "homography": geom.homography,
            "lane_assignment_anchor": ROI_CONFIG.lane_assignment_anchor,
        },
        "lane_assignment_status": lane_status,
        "lane_assignment_detail": geom.lane_assignment_reason,
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
        "night_enhancement_applied": night_applied,
        "frame_luma": {
            "mean": round(night_luma_meta["luma_mean"], 2),
            "median": round(night_luma_meta["luma_median"], 2),
        },
    }

    if enhanced_preview:
        ok_enc, enc_buf = cv2.imencode(".jpg", inference_image, [int(cv2.IMWRITE_JPEG_QUALITY), 78])
        if ok_enc:
            payload["enhanced_preview_jpeg_b64"] = base64.b64encode(enc_buf.tobytes()).decode("ascii")

    if debug_overlay:
        payload["debug_overlay_jpeg_b64"] = _debug_overlay(
            image=image,
            tracks=(mature_roi_tracks if mature_roi_tracks else tracks),
            geom=geom,
            queue_like=bool(queue_like),
            stopped_like=bool(stopped_like),
        )
    return payload
