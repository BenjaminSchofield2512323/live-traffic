# Detector sidecar (FastAPI)

The Go API (`backend/cmd/api`) calls **`POST /internal/detect`** on this service (default **`http://localhost:8090`**). Set **`DETECTOR_BASE_URL`** if you use another host/port.

## Quick start (macOS / Linux)

```bash
cd detector
python3 -m venv .venv
source .venv/bin/activate   # Windows: .venv\Scripts\activate
pip install -r requirements.txt
uvicorn app:app --host 127.0.0.1 --port 8090
```

Verify:

```bash
curl -sS -o /dev/null -w "%{http_code}\n" http://127.0.0.1:8090/docs
```

Then start the Go API (in another terminal):

```bash
cd backend
go run ./cmd/api
```

## What this ships with

- **`POST /internal/detect`** — accepts **raw JPEG bytes** (`Content-Type: application/octet-stream`), optional query params `stream_id`, `imgsz`, `conf`. Returns JSON with `detections: []` (stub) and image dimensions.
- **`GET /healthz`** — liveness.

Replace `app.py` inference with YOLO when ready; keep the route and a JSON shape your UI can consume.
