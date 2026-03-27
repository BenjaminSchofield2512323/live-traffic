# Backend (Go) - Live Traffic API

This service is the control-plane API for v1.

## What it does now

- Uses 511NY's internal data endpoint instead of scraping rendered HTML.
- Primes session with `GET /cctv` and then fetches:
  - `GET /List/GetData/Cameras?lang=en&query=...`
- Returns camera lists and recommended camera picks with two-phase live-stream selection.

## Endpoints

- `GET /healthz`
- `GET /api/v1/cameras?start=0&length=25`
- `GET /api/v1/cameras/recommended?count=5`
- `POST /api/v1/pipeline/start?camera_count=5`
- `GET /api/v1/pipeline/focus/stream?mode=live|processed`
- `POST /api/v1/pipeline/focus/detect?stream_id=cam-57&imgsz=640&conf=0.25`
- `GET /api/v1/analysis/plan`

## Run locally

```bash
cd backend
go run ./cmd/api
```

Default bind: `:8080`

## Notes

- Recommendation flow:
  - ranks video-eligible feeds by corridor priority
  - phase 1 keeps feeds that pass a lightweight HLS liveness probe
  - phase 2 fills any remaining slots from the same ranked list (no probe requirement)
- Focus detect flow:
  - fetches current focus snapshot from selected camera
  - forwards JPEG to detector sidecar at `${DETECTOR_BASE_URL}/internal/detect`
  - tracks workflow progress and cooldown to avoid detector request floods
- Inference is intentionally split for future sidecar workers:
  - Go API: orchestration and metrics/alerts API
  - Python worker: YOLO + tracking
