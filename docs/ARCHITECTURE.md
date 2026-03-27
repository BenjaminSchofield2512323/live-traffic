# Live Traffic Camera Analysis - Architecture (V1)

This project is a fast-first traffic intelligence MVP using:

- 511NY camera APIs as upstream feed metadata source
- Go backend for orchestration and APIs
- React frontend for operations dashboard
- AWS deployment with cheapest acceptable defaults

## Why endpoint-based ingestion (not scraping)

511NY exposes camera metadata and playable video URLs through the same APIs used by its UI:

- `GET /List/GetData/Cameras`
- Returns camera records with:
  - `images[].videoUrl` (HLS playlist)
  - `images[].imageUrl` (still image endpoint)
  - camera location/roadway metadata

The backend reproduces required headers/session behavior so ingestion is stable and less brittle than parsing HTML cards.

## Core components

1. **Camera Catalog Service (Go)**
   - primes session via `/cctv`
   - fetches cameras from `/List/GetData/Cameras`
   - scores cameras for V1 recommendation set (5-10)

2. **Detection/Scoring Engine (implemented v1 in Go)**
   - consumes rolling camera snapshots
   - computes lightweight motion + occupancy heuristics
   - emits:
     - congestion spike alerts
     - stopped vehicle candidates
     - queue growth alerts
     - camera offline alerts
   - captures evidence artifacts per alert:
     - pre-event snapshot
     - post-event snapshot
     - event GIF clip

3. **Model Worker (planned, optional sidecar)**
   - future YOLO + tracking for higher precision under scale
   - can replace or augment heuristic scoring

4. **Metrics and Storage (planned)**
   - hot path: Redis (short TTL)
   - durable path: Postgres/Timescale
   - optional media retention:
     - raw clips/snapshots in S3
     - lifecycle policy for cost control

5. **Frontend (React)**
   - pipeline controls (start/stop/status)
   - live alert feed with evidence clip links
   - camera shortlist and health context

## Latency goals

- Camera metadata refresh: 30-60s
- Inference cadence: 2-5 FPS per camera
- Alert latency target: p95 < 3 seconds

## AWS cost-first deployment defaults

- **API / frontend**: ECS Fargate, single task each
- **Worker**:
  - start CPU-only if camera count is low and resolution is limited
  - move to one small GPU task when p95 latency target is missed
- **DB**: smallest production-safe RDS class with automated backups
- **Cache**: smallest ElastiCache tier or in-memory fallback for early tests
- **S3**: enable compression and lifecycle transitions/deletion

## GPU decision guidance

You do not always need GPU for 5-10 cameras if:

- you run low FPS (2-3)
- resize frames aggressively
- keep only essential classes (vehicles)

You likely need GPU when:

- camera count increases
- FPS target rises
- you require near-real-time alerts under load with high resolution streams

## Alert model (V1)

- Congestion score spike
- Stopped vehicle candidate
- Queue growth acceleration
- Camera offline / no-frame signal

