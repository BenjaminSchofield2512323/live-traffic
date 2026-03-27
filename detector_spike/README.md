# Detector Spike (Milestone 1)

This directory contains milestone-1 implementation for detector-based metrics.

It does **not** replace existing Go heuristic signals yet; it runs in parallel as a validation path.

## What is implemented

- Python detector sidecar (`FastAPI`) with `POST /internal/detect`
- YOLO default model (`yolov8n.pt`) on CPU by default
- Object-based metrics:
  - `occupancy_ratio` (ROI pixel coverage by vehicle detections)
  - `vehicle_count`
  - `moving_vehicle_count`
  - `moving_ratio`
  - `mean_track_speed_px_s`
- Minimal nearest-neighbor centroid tracker keyed by `stream_id`
- Scripts to:
  - pull a camera snapshot from Go API and call detector
  - run repeated calls for latency probe

## Install

```bash
python3 -m venv .venv
source .venv/bin/activate
pip install -r detector_spike/requirements.txt
```

## Run detector sidecar

```bash
source .venv/bin/activate
uvicorn detector_spike.app:app --host 0.0.0.0 --port 8090
```

Optional environment:

- `DETECTOR_MODEL` (default `yolov8n.pt`)
- `DETECTOR_DEVICE` (default `cpu`)
- `DETECTOR_TRACK_TTL_SEC` (default `4.0`)
- `DETECTOR_TRACK_MAX_MATCH_PX` (default `70.0`)

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

Optional query params:

- `roi` polygon points CSV (`x1,y1,x2,y2,...`)
- `moving_speed_threshold_px_s` (default `12.0`)
- `iou` (default `0.45`)

## End-to-end sample from pipeline snapshots

Prereq:

1. run backend (`go run ./backend/cmd/api`)
2. start pipeline:

```bash
curl -X POST "http://localhost:8080/api/v1/pipeline/start" \
  -H "Content-Type: application/json" \
  -d '{"camera_count":10,"sample_interval_sec":3,"buffer_seconds":90,"pre_event_seconds":30,"post_event_seconds":45}'
```

3. choose an active camera id from:

```bash
curl "http://localhost:8080/api/v1/pipeline/cameras"
```

Then run:

```bash
source .venv/bin/activate
python detector_spike/scripts/detect_from_camera_snapshot.py \
  --camera-id 57 \
  --api-base http://localhost:8080 \
  --detector-base http://localhost:8090 \
  --output detector_spike/samples/detect_sample_output.json
```

Latency probe:

```bash
python detector_spike/scripts/latency_probe.py \
  --camera-id 57 \
  --iterations 12 \
  --api-base http://localhost:8080 \
  --detector-base http://localhost:8090 \
  --output detector_spike/samples/detect_latency_probe.json
```

## Milestone boundary

- Included now: ADR + detector service + sample client path + sample output/latency artifacts.
- Deferred to milestone 2: wiring detector outputs into Go API and frontend alongside old heuristics.
