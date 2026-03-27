# Backend (Go) - Live Traffic API

This service is the control-plane API for v1.

## What it does now

- Uses 511NY's internal data endpoint instead of scraping rendered HTML.
- Primes session with `GET /cctv` and then fetches:
  - `GET /List/GetData/Cameras?lang=en&query=...`
- Returns camera lists and recommended 5-10 camera picks with live `videoUrl`.

## Endpoints

- `GET /healthz`
- `GET /api/v1/cameras?start=0&length=25`
- `GET /api/v1/cameras/recommended?count=10`
- `GET /api/v1/analysis/plan`

## Run locally

```bash
cd backend
go run ./cmd/api
```

Default bind: `:8080`

## Notes

- Current recommendation logic is deterministic and corridor-priority based.
- Inference is intentionally split for future sidecar workers:
  - Go API: orchestration and metrics/alerts API
  - Python worker: YOLO + tracking
