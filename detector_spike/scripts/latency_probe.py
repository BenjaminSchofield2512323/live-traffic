from __future__ import annotations

import argparse
import json
import pathlib
import statistics
import time

import requests


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description="Run repeated detector calls to measure latency.")
    p.add_argument("--api-base", default="http://localhost:8080", help="Go API base URL")
    p.add_argument("--detector-base", default="http://localhost:8090", help="Detector API base URL")
    p.add_argument("--camera-id", type=int, required=True, help="Camera ID active in pipeline")
    p.add_argument("--mode", default="raw", choices=["processed", "raw"], help="Snapshot mode")
    p.add_argument("--iterations", type=int, default=12, help="Number of detector requests")
    p.add_argument("--imgsz", type=int, default=640, help="Detector input size")
    p.add_argument("--conf", type=float, default=0.25, help="Detector confidence threshold")
    p.add_argument(
        "--output",
        default="detector_spike/samples/detect_latency_probe.json",
        help="Path to write latency report JSON",
    )
    return p.parse_args()


def percentile(values: list[float], p: float) -> float:
    if not values:
        return 0.0
    idx = max(0, min(len(values) - 1, int(round((p / 100.0) * (len(values) - 1)))))
    return sorted(values)[idx]


def main() -> int:
    args = parse_args()
    snapshot_url = f"{args.api_base}/api/v1/pipeline/focus/snapshot?camera_id={args.camera_id}&mode={args.mode}"
    roundtrip_ms: list[float] = []
    inference_ms: list[float] = []
    vehicle_counts: list[int] = []

    for i in range(args.iterations):
        snap_resp = requests.get(snapshot_url, timeout=12)
        snap_resp.raise_for_status()
        image_bytes = snap_resp.content
        if not image_bytes:
            raise RuntimeError("snapshot endpoint returned empty image bytes")

        detect_url = (
            f"{args.detector_base}/internal/detect"
            f"?stream_id=cam-{args.camera_id}&imgsz={args.imgsz}&conf={args.conf}"
        )
        t0 = time.perf_counter()
        detect_resp = requests.post(detect_url, data=image_bytes, timeout=45)
        rt_ms = (time.perf_counter() - t0) * 1000.0
        detect_resp.raise_for_status()
        payload = detect_resp.json()
        inf = float(payload.get("inference_ms", 0.0))
        vc = int(payload.get("metrics", {}).get("vehicle_count", 0))

        roundtrip_ms.append(rt_ms)
        inference_ms.append(inf)
        vehicle_counts.append(vc)
        print(f"[{i + 1}/{args.iterations}] roundtrip_ms={rt_ms:.2f} inference_ms={inf:.2f} vehicles={vc}")

    summary = {
        "sample_input": {
            "api_base": args.api_base,
            "detector_base": args.detector_base,
            "camera_id": args.camera_id,
            "mode": args.mode,
            "iterations": args.iterations,
            "imgsz": args.imgsz,
            "conf": args.conf,
        },
        "roundtrip_ms": {
            "avg": round(statistics.mean(roundtrip_ms), 2),
            "p50": round(percentile(roundtrip_ms, 50), 2),
            "p95": round(percentile(roundtrip_ms, 95), 2),
            "max": round(max(roundtrip_ms), 2),
        },
        "inference_ms": {
            "avg": round(statistics.mean(inference_ms), 2),
            "p50": round(percentile(inference_ms, 50), 2),
            "p95": round(percentile(inference_ms, 95), 2),
            "max": round(max(inference_ms), 2),
        },
        "vehicle_count": {
            "avg": round(statistics.mean(vehicle_counts), 2),
            "min": min(vehicle_counts),
            "max": max(vehicle_counts),
        },
    }

    out_path = pathlib.Path(args.output)
    out_path.parent.mkdir(parents=True, exist_ok=True)
    out_path.write_text(json.dumps(summary, indent=2), encoding="utf-8")
    print(f"Wrote latency probe report to {out_path}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
