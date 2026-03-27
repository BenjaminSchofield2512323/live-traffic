# Detector Spike (MOT + ROI v1)

This sidecar exposes `POST /internal/detect` and now includes a known MOT path on top of YOLO
(Ultralytics tracking mode with ByteTrack config by default), plus ROI/lane-aware aggregates.

## One-line priority

Implement known MOT on top of YOLO + ROI + smoothing; ship tracks + lane aggregates in the detector response—boxes alone are not enough.

## What is implemented

- YOLO inference with Ultralytics `model.track(..., tracker=...)`
- Per-stream persistent tracking keyed by `stream_id`
  - stable `track_id` within a session
  - bbox, class, confidence
  - centroid/bottom-center
  - velocity vector (`vx`, `vy`) and smoothed speed proxy (`speed_px_s`)
- Temporal smoothing window for speed and stop/queue heuristics
- ROI/lane model:
  - road polygon + direction vector
  - optional lane polygons
  - tracks outside ROI are ignored for lane aggregates
- Backward compatibility:
  - `detections[]` remains in response for current UI consumers
  - enriched fields are added (`track_id`, `lane_id`, smoothed speed)
- Optional debug overlay (base64 JPEG) with `track_id` labels for QA

## Install

```bash
python3 -m venv .venv
source .venv/bin/activate
pip install -r detector_spike/requirements.txt
```

## Run

```bash
source .venv/bin/activate
uvicorn detector_spike.app:app --host 0.0.0.0 --port 8090
```

## API

### Health

```bash
curl "http://localhost:8090/internal/health"
```

### Detect

```bash
curl -X POST "http://localhost:8090/internal/detect?stream_id=cam-57&imgsz=640&conf=0.25" \
  --data-binary "@/path/to/frame.jpg"
```

Common query params:

- `stream_id`, `conf`, `iou`, `imgsz`
- `roi` (CSV polygon `x1,y1,x2,y2,...`)
- `lanes` (JSON list: `[{"id":"lane_1","polygon":[[x,y],...]}]`)
- `direction` (CSV vector, e.g. `1,0`)
- `moving_speed_threshold_px_s`
- `smoothing_window_sec`
- `debug_overlay=true|false`

## ROI/lane configuration per camera

Use `DETECTOR_ROI_CONFIG_PATH` to load a JSON config:

```json
{
  "default": {
    "road_polygon": [[0, 200], [1280, 200], [1280, 720], [0, 720]],
    "direction": [1, 0],
    "lanes": [
      {"id": "lane_1", "polygon": [[0, 200], [420, 200], [420, 720], [0, 720]]},
      {"id": "lane_2", "polygon": [[420, 200], [860, 200], [860, 720], [420, 720]]},
      {"id": "lane_3", "polygon": [[860, 200], [1280, 200], [1280, 720], [860, 720]]}
    ]
  },
  "cam-57": {
    "direction": [0.97, 0.24]
  }
}
```

`stream_id` is used as the key (`cam-57`, etc). If missing, `default` is used.

## Environment knobs

- `DETECTOR_MODEL` (default `yolov8n.pt`)
- `DETECTOR_DEVICE` (default `cpu`)
- `DETECTOR_TRACKER_CONFIG` (default `bytetrack.yaml`)
- `DETECTOR_TRACK_MAX_AGE_SEC` (default `4.0`)
- `DETECTOR_TRACK_MIN_HITS` (default `2`)
- `DETECTOR_SMOOTH_WINDOW_SEC` (default `1.0`)
- `DETECTOR_STOPPED_SPEED_THRESHOLD_PX_S` (default `2.0`)
- `DETECTOR_STOPPED_MIN_SEC` (default `20.0`)
- `DETECTOR_QUEUE_SPEED_THRESHOLD_PX_S` (default `8.0`)
- `DETECTOR_QUEUE_OCCUPANCY_THRESHOLD` (default `0.18`)
- `DETECTOR_QUEUE_MIN_TRACKS` (default `4`)
- `DETECTOR_ROI_CONFIG_PATH` (optional path to ROI/lane JSON)
- `DETECTOR_DEBUG_OVERLAY_DEFAULT` (`false` by default)

## Response contract highlights

For each request:

- `stream_id`
- `frame_seq`
- `frame_ts_unix_ms`
- `detections[]` (backward-compatible)
- `tracks[]` (new richer tracking output)
- `geometry`:
  - `road_polygon`
  - `direction`
  - `lanes[]` (`lane_id`, `polygon`) for frontend/debug overlays
- `metrics`:
  - `counts_per_lane`, `counts_per_roi`
  - `mean_smoothed_speed_px_s`, `median_smoothed_speed_px_s`
  - `queue_like`, `stopped_like`

## Limitations (v1)

- No cross-camera re-identification
- Speed is image-space proxy (`px/s`) unless calibrated
- Night/rain/occlusions can degrade track continuity
- `queue_like` / `stopped_like` are heuristics, not incident truth
