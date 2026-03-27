from __future__ import annotations

import argparse
import json
import pathlib
import time

import requests


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description="Fetch one pipeline focus snapshot and run detector sidecar /internal/detect."
    )
    p.add_argument("--api-base", default="http://localhost:8080", help="Go API base URL")
    p.add_argument("--detector-base", default="http://localhost:8090", help="Detector API base URL")
    p.add_argument("--camera-id", type=int, required=True, help="Camera ID active in pipeline")
    p.add_argument("--mode", default="processed", choices=["processed", "raw"], help="Snapshot mode")
    p.add_argument("--stream-id", default="", help="Stream ID for tracker continuity")
    p.add_argument("--imgsz", type=int, default=640, help="Detector input size")
    p.add_argument("--conf", type=float, default=0.25, help="Detector confidence threshold")
    p.add_argument(
        "--output",
        default="detector_spike/samples/detect_sample_output.json",
        help="Path to write output JSON",
    )
    return p.parse_args()


def main() -> int:
    args = parse_args()
    stream_id = args.stream_id or f"cam-{args.camera_id}"
    snapshot_url = f"{args.api_base}/api/v1/pipeline/focus/snapshot?camera_id={args.camera_id}&mode={args.mode}"
    snap_resp = requests.get(snapshot_url, timeout=12)
    snap_resp.raise_for_status()
    image_bytes = snap_resp.content
    if not image_bytes:
        raise RuntimeError("snapshot endpoint returned empty image bytes")

    detect_url = (
        f"{args.detector_base}/internal/detect"
        f"?stream_id={stream_id}&imgsz={args.imgsz}&conf={args.conf}"
    )
    t0 = time.perf_counter()
    detect_resp = requests.post(detect_url, data=image_bytes, timeout=45)
    elapsed_ms = (time.perf_counter() - t0) * 1000.0
    detect_resp.raise_for_status()
    payload = detect_resp.json()
    payload["client_roundtrip_ms"] = round(elapsed_ms, 2)
    payload["sample_input"] = {
        "api_base": args.api_base,
        "detector_base": args.detector_base,
        "camera_id": args.camera_id,
        "mode": args.mode,
        "stream_id": stream_id,
        "snapshot_bytes_len": len(image_bytes),
    }

    out_path = pathlib.Path(args.output)
    out_path.parent.mkdir(parents=True, exist_ok=True)
    out_path.write_text(json.dumps(payload, indent=2), encoding="utf-8")
    print(f"Wrote detector sample output to {out_path}")
    print(
        f"inference_ms={payload.get('inference_ms')} "
        f"client_roundtrip_ms={payload.get('client_roundtrip_ms')} "
        f"vehicle_count={payload.get('metrics', {}).get('vehicle_count')}"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
