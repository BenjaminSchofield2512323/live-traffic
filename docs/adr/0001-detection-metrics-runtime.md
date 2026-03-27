# ADR-0001: Detection-based Metrics Runtime and Definitions

- Status: Accepted (milestone 1 spike)
- Date: 2026-03-27
- Owners: Traffic camera pipeline team

## Context

Current `motion` and `occupancy` values are pixel-heuristic signals from a low-resolution grayscale diff and edge density method (`backend/cmd/api/pipeline.go` and `frontend/src/focusFrameMetrics.js`).

These are useful for a first-pass baseline, but they are not calibrated traffic metrics. In heavy traffic, low motion and stable occupancy values can still appear, which makes product-level interpretation difficult.

We need object/track-based metrics that are explicit and interpretable.

## Decision

For milestone 1, run detection inference in a **server-side Python sidecar** with a minimal HTTP API:

- Runtime: `FastAPI` + `ultralytics` model runner (`yolov8n.pt` default), CPU-first
- Endpoint: `POST /internal/detect` with image bytes
- Output: detection boxes/classes and derived object-based metrics

The Go pipeline remains unchanged for alert decisions during this milestone. Detector outputs are additive and for validation/spike learning.

## Why this runtime

### Chosen: Python sidecar (server-side)

Pros:
- Fastest path to a working detector with battle-tested model ecosystem
- Easy model swaps (YOLO variants or alternatives) with minimal API contract change
- Keeps browser thin and avoids client hardware/browser variability
- Avoids embedding heavy ML runtime into Go process in milestone 1

Cons:
- Additional service to run/monitor
- Inter-service latency overhead

### Not chosen now

1. Go calling ONNX directly:
   - Viable later for packaging simplification, but slower to spike and tune now.
2. Browser inference:
   - Client-dependent performance and no server-grade consistency.
3. Replace Go heuristics immediately:
   - Increases migration risk before metrics are validated.

## Metric Definitions (explicit)

Definitions are for the spike service response payload:

1. `occupancy_ratio`
   - Definition: fraction of ROI pixels covered by the union of vehicle detection boxes.
   - Formula: `occupied_px / roi_area_px`.
   - Vehicle classes in spike: `car`, `truck`, `bus`, `motorcycle`.

2. `vehicle_count`
   - Definition: number of vehicle detections in frame after class filtering.

3. `moving_vehicle_count`
   - Definition: tracked vehicles with speed above `moving_speed_threshold_px_s`.
   - Tracking method in spike: nearest-neighbor centroid matching between consecutive requests by `stream_id`.

4. `moving_ratio`
   - Definition: `moving_vehicle_count / vehicle_count` (0 when no vehicles).

5. `mean_track_speed_px_s`
   - Definition: average centroid speed (pixels/sec) across active tracked detections in the current request.

Notes:
- Speed is currently pixel-space, camera-dependent, and not yet world-space calibrated.
- ROI defaults to full frame and may be provided as polygon query param.

## Performance Targets (milestone 1)

Targets are for a **single camera path** on CPU with `yolov8n`:

- Detector API p95 roundtrip (image POST to response): `< 900ms` at `imgsz=640`
- Model inference p95: `< 700ms` CPU baseline
- Sustained per-camera throughput target for spike path: `~1-2 FPS`

This milestone is not a 30 FPS detector rollout. Focus UI and pipeline continue existing behavior while detector metrics are validated.

## GPU/CPU Expectations

- Default: CPU-first sidecar (`DETECTOR_DEVICE=cpu`)
- GPU optional when:
  - camera count scales beyond spike scope,
  - detector cadence rises materially,
  - p95 latency misses product targets.

## Rollout Plan

1. Milestone 1 (this change): ADR + detector spike service + sample client + latency proof.
2. Milestone 2: integrate detector metrics into Go pipeline/focus API alongside heuristics (feature flag or side-by-side fields).
3. Milestone 3: promote detector/track metrics to alert logic and deprecate heuristic fields once validated.

