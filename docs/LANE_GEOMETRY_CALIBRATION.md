# Lane geometry calibration (phase 1)

This project now supports per-camera lane geometry as source-of-truth for lane assignment.

## Config location

Default sample config is at:

- `detector_spike/config/lane_geometry.v1.json`

Set path at runtime:

- `DETECTOR_ROI_CONFIG_PATH=/absolute/path/to/lane_geometry.v1.json`

## Coordinate system

The detector expects lane polygons in **detector input image coordinates**:

- same width/height as image bytes posted to `/internal/detect`
- by default in this app, that is the resized focus frame path (`imgsz=640`) preserving aspect ratio

Do not provide full-source-resolution polygons unless those exact pixels are sent to detector.

## Schema (versioned)

Top-level keys:

- `schema_version`: required, currently `lane-geometry-v1`
- `detector_input_resolution`: optional `{ "width": <int>, "height": <int> }`
- `lane_assignment_anchor`: optional (`bbox_bottom_center` default)
- `overlap_tiebreak`: optional (`config_order` default, `largest_area` supported)
- `streams`: required object keyed by `stream_id` (for example `cam-49`)

Per stream:

- `road_polygon`: optional polygon `[[x,y], ...]`
- `lanes`: required non-empty list:
  - `{ "lane_id": "lane_1", "polygon": [[x,y], ...] }`
- `direction`: optional vector `[dx, dy]`
- `homography`: optional 3x3 matrix (stored for forward compatibility)

See `detector_spike/config/lane_geometry.v1.json` for an example including a golden stream.

## Assignment rule

The detector assigns lane using:

1. assignment anchor from config (default: bottom-center point of each tracked bbox)
2. point-in-polygon against each lane polygon
3. if multiple lanes overlap, use configured tie-break:
   - `config_order` (default)
   - `largest_area`
4. if not inside any lane, `lane_id = null` and lane counts ignore that track

## Fail-closed behavior

- Invalid config/schema -> geometry is treated as unavailable
- detector does not crash
- lane metrics degrade gracefully:
  - empty `geometry.lanes`
  - track/detection `lane_id = null`
  - empty `counts_per_lane`

No heuristic trapezoids are injected when config is invalid/missing.

## How to create/edit geometry

1. Take a representative frame for a stream.
2. Use an internal annotation flow or labeling tool to mark lane polygons.
3. Export polygons as pixel points and write them into stream config.
4. Restart detector sidecar and verify in focus overlay.

## Quick validation checklist

For one calibrated stream:

1. Open focused view and confirm lane polygons align with road markings.
2. Observe one vehicle over consecutive frames: lane_id should remain stable under normal bbox jitter.
3. Change only config polygons and restart detector: overlay + lane counts should change without code edits.

