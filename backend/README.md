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
- `GET /api/v1/cameras/recommended?count=5`
- `POST /api/v1/pipeline/start?camera_count=5`
- `GET /api/v1/pipeline/focus/stream?mode=live|processed`
- `POST /api/v1/pipeline/focus/detect?stream_id=cam-57&imgsz=640&conf=0.25`
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

- Recommendation flow:
  - ranks video-eligible feeds by corridor priority
  - phase 1 keeps feeds that pass a lightweight HLS liveness probe
  - phase 2 fills any remaining slots from the same ranked list (no probe requirement)
- Focus detect flow:
  - fetches current focus snapshot from selected camera
  - forwards JPEG to detector sidecar at `${DETECTOR_BASE_URL}/internal/detect`
  - supports detector tuning passthrough query params:
    - `iou`, `roi`, `lanes`, `direction`, `moving_speed_threshold_px_s`,
      `smoothing_window_sec`, `debug_overlay`
  - tracks workflow progress and cooldown to avoid detector request floods
- Inference is intentionally split for future sidecar workers:
  - Go API: orchestration and metrics/alerts API
  - Python worker: YOLO + tracking
