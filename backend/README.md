# Backend (Go) - Incident Intelligence API

This service now implements a full v1 pipeline:

1. ingest camera metadata + snapshots from 511NY
2. keep a rolling per-camera frame buffer
3. run lightweight heuristics for incident signals
4. score and suppress noisy candidates
5. trigger evidence artifacts (before/after + clip)
6. output structured alerts and optional webhook

## Data source strategy

Uses 511NY endpoint flow (same path behind "Show Video"), not HTML scraping:

- session prime: `GET /cctv`
- camera data: `GET /List/GetData/Cameras?lang=en&query=...`
- image pulls: camera `images[].imageUrl`

## Endpoints

- `GET /healthz`
- `GET /api/v1/cameras?start=0&length=25`
- `GET /api/v1/cameras/recommended?count=10`
- `GET /api/v1/analysis/plan`
- `POST /api/v1/pipeline/start`
- `POST /api/v1/pipeline/stop`
- `GET /api/v1/pipeline/status`
- `GET /api/v1/pipeline/cameras`
- `GET /api/v1/pipeline/focus/stream?camera_id=<id>&mode=processed|raw&fps=30`
- `POST /api/v1/pipeline/focus/detect?camera_id=<id>&stream_id=cam-<id>&imgsz=640&conf=0.25`
- `GET /api/v1/alerts?limit=100`
- `GET /artifacts/...` (evidence files)

## Maintainability notes

The backend is organized into smaller modules to keep frame-analysis changes safer:

- `main.go`: HTTP route wiring + server bootstrap
- `pipeline.go`: runtime loop, buffering, scoring, alert triggering
- `camera_selection.go`: camera catalog pagination + live-selection helpers
- `http_utils.go`: shared JSON/CORS request helpers
- `config_utils.go`: query/config parsing helpers
- `metrics_utils.go`: shared metric utilities used by signal scoring
- `math_utils.go`: shared numeric helpers (clamp/min/max)

If you modify frame analysis logic, prefer changing pure helpers first (`evaluateSignals`, metric helpers) and keep orchestration code in `processCamera` minimal.

## Run locally

```bash
cd backend
go run ./cmd/api
```

Default bind: `:8080`

## One-command local dev

From repo root:

```bash
make setup
make dev
```

This runs backend (`:8080`), frontend (`:5173`), and detector sidecar (`:8090`) together.

## Pipeline defaults

- cameras: 10 (bounded 5-10)
- sample interval: 3s
- rolling buffer: 90s
- clip window: 30s pre-event + 45s post-event

## Start/stop examples

Start pipeline:

```bash
curl -X POST "http://localhost:8080/api/v1/pipeline/start" \
  -H "Content-Type: application/json" \
  -d '{
    "camera_count": 10,
    "sample_interval_sec": 3,
    "buffer_seconds": 90,
    "pre_event_seconds": 30,
    "post_event_seconds": 45
  }'
```

Fetch alert feed:

```bash
curl "http://localhost:8080/api/v1/alerts?limit=20"

# Camera live-processing views (raw + processed frame URLs)
curl "http://localhost:8080/api/v1/pipeline/cameras"

# Focus stream (MJPEG, defaults to processed mode and 30 FPS cap)
curl "http://localhost:8080/api/v1/pipeline/focus/stream?camera_id=57&mode=processed&fps=30"

# Focus detector proxy (for UI overlay). Body is JPEG/PNG bytes.
curl -X POST \
  "http://localhost:8080/api/v1/pipeline/focus/detect?camera_id=57&stream_id=cam-57&imgsz=640&conf=0.25" \
  --data-binary "@/tmp/frame.jpg"
```

## Detector sidecar integration

When `DETECTOR_BASE_URL` points to the Python sidecar (default `http://localhost:8090`),
the focus UI can render YOLO boxes + detector metrics on the processed canvas.

This path is throttled on the frontend to avoid trying YOLO at full 30 FPS.

Stop pipeline:

```bash
curl -X POST "http://localhost:8080/api/v1/pipeline/stop"
```

## Webhook

Set `ALERT_WEBHOOK_URL` to receive POSTed JSON alert payloads.

## Artifact storage

Set `ARTIFACT_DIR` (default `./artifacts`).

For each alert, files are written to:

- `before.jpg`
- `after.jpg`
- `event.gif`
