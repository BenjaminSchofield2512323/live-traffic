from __future__ import annotations

import unittest

from detector_spike.app import MOTStateStore


def _det(x1: float, y1: float, x2: float, y2: float) -> dict:
    return {
        "bbox": [x1, y1, x2, y2],
        "class_id": 2,
        "class_name": "car",
        "confidence": 0.9,
        "_raw_track_id": None,
    }


def _tracks_to_map(tracks: list[dict]) -> dict[int, list[float]]:
    out: dict[int, list[float]] = {}
    for t in tracks:
        out[int(t["track_id"])] = [float(v) for v in t["bbox"]]
    return out


class TestHungarianAssociation(unittest.TestCase):
    def test_hungarian_global_assignment_prefers_consistent_pairing(self) -> None:
        """
        Synthetic case where greedy list-order matching would swap IDs:
        - Track 1 best matches det A and second-best det B
        - Track 2 matches det B reasonably
        Greedy (processing det B first) can assign det B->track1 and force det A->track2.
        Hungarian should choose det A->track1 and det B->track2 globally.
        """

        store = MOTStateStore(
            track_max_age_sec=10.0,
            smooth_window_sec=1.0,
            min_hits=1,
            assoc_iou_threshold=0.0,
            bbox_ema_alpha=0.0,
            assoc_center_max_px=0.0,
            assoc_hungarian_enabled=True,
        )
        stream = "test-hungarian"
        direction = (1.0, 0.0)

        # Seed two tracks.
        first = store.update_tracks(
            stream_id=stream,
            detections=[_det(0, 0, 10, 10), _det(20, 0, 30, 10)],
            direction=direction,
            now_ts=1.0,
        )
        seeded = _tracks_to_map(first)
        self.assertEqual(set(seeded.keys()), {1, 2})

        # Order detections so greedy tends to steal track 1 first.
        # det_b is closer to track 1 than track 2, but det_a is much better for track 1.
        second = store.update_tracks(
            stream_id=stream,
            detections=[_det(6, 0, 16, 10), _det(0, 0, 10, 10)],
            direction=direction,
            now_ts=2.0,
        )

        # Find which track got the exact det_a box [0,0,10,10].
        det_a_track = None
        for t in second:
            if [round(v, 2) for v in t["bbox"]] == [0.0, 0.0, 10.0, 10.0]:
                det_a_track = int(t["track_id"])
                break

        self.assertEqual(det_a_track, 1)

    def test_empty_detections_no_regression(self) -> None:
        store = MOTStateStore(
            track_max_age_sec=10.0,
            smooth_window_sec=1.0,
            min_hits=1,
            assoc_iou_threshold=0.25,
            bbox_ema_alpha=0.2,
            assoc_center_max_px=32.0,
            assoc_hungarian_enabled=True,
        )
        tracks = store.update_tracks(
            stream_id="empty",
            detections=[],
            direction=(1.0, 0.0),
            now_ts=1.0,
        )
        self.assertEqual(tracks, [])
