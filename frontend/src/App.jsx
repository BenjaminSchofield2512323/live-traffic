import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import './App.css'
import { FocusStreamFrames } from './FocusStreamFrames.jsx'

// In `vite` dev, default to same-origin `/api` (see vite.config.js proxy → :8080) so a stray
// VITE_API_BASE_URL (e.g. wrong port) does not break fetches. To call the API directly in dev,
// set VITE_DEV_DIRECT_API=1 and VITE_API_BASE_URL=http://host:port
const apiBase =
  import.meta.env.DEV && import.meta.env.VITE_DEV_DIRECT_API !== '1'
    ? ''
    : import.meta.env.VITE_API_BASE_URL || 'http://localhost:8080'
const focusRefreshIntervalMs = 2000
const laneStorageKey = 'focus-lane-polygons-v1'
const laneFlowWindowSec = 60

function loadLaneGeometryFromStorage() {
  try {
    if (typeof window === 'undefined') return {}
    const raw = window.localStorage.getItem(laneStorageKey)
    if (!raw) return {}
    const parsed = JSON.parse(raw)
    if (!parsed || typeof parsed !== 'object') return {}
    return parsed
  } catch {
    return {}
  }
}

function saveLaneGeometryToStorage(payload) {
  try {
    if (typeof window === 'undefined') return
    window.localStorage.setItem(laneStorageKey, JSON.stringify(payload))
  } catch {
    // Browser storage can be unavailable (private mode/quota). Keep app usable.
  }
}

function buildImageURL(path) {
  if (!path) return ''
  if (path.startsWith('http://') || path.startsWith('https://')) return path
  return `https://511ny.org${path}`
}

function App() {
  const [recommended, setRecommended] = useState([])
  const [alerts, setAlerts] = useState([])
  const [cameraViews, setCameraViews] = useState([])
  const [focusCameraID, setFocusCameraID] = useState(null)
  const [pipelineStatus, setPipelineStatus] = useState(null)
  const [analysisPlan, setAnalysisPlan] = useState(null)
  const [detectStatus, setDetectStatus] = useState({
    phase: 'idle',
    inProgress: false,
    detectorAvailable: false,
    consecutiveFailures: 0,
    retryAfterMs: 0,
    lastError: '',
    lastDurationMs: 0,
    detectionsCount: 0,
    tracksCount: 0,
    queueLike: false,
    stoppedLike: false,
    meanSmoothedSpeedPxS: 0,
  })
  const [focusCacheBust, setFocusCacheBust] = useState(Date.now())
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [starting, setStarting] = useState(false)
  const [stopping, setStopping] = useState(false)
  const [focusFPS, setFocusFPS] = useState(30)
  const [detectorFPS, setDetectorFPS] = useState(5)
  /** Detector-side metrics from YOLO sidecar via backend focus proxy. */
  const [detectorMetrics, setDetectorMetrics] = useState(null)
  /** Per-camera geometry authored in the focus view and cached in browser storage. */
  const [laneGeometryByCamera, setLaneGeometryByCamera] = useState(() => loadLaneGeometryFromStorage())
  const [laneEditMode, setLaneEditMode] = useState(false)
  const [laneFlowStats, setLaneFlowStats] = useState({ totalPerMin: 0, perLane: {} })
  const [trackedEntities, setTrackedEntities] = useState([])
  const flowStateRef = useRef({})
  const overlayIDMapRef = useRef({})

  /** Focus canvas POSTs to /focus/detect; sync metrics cards + lane counts from the same payload the overlay uses. */
  const onDetectionMetrics = useCallback((payload) => {
    setDetectorMetrics(payload)
    if (!payload) {
      setTrackedEntities([])
      return
    }
    const metrics = payload.metrics || {}
    const detections = Array.isArray(payload.detections) ? payload.detections : []
    const tracks = Array.isArray(payload.tracks) ? payload.tracks : []
    const localLanes =
      focusCameraID && laneGeometryByCamera[String(focusCameraID)]?.lanes
        ? laneGeometryByCamera[String(focusCameraID)].lanes
        : []
    const localLaneIDs = new Set(
      localLanes
        .map((lane) => String(lane?.lane_id || lane?.id || '').trim())
        .filter(Boolean),
    )
    const streamID = String(payload.stream_id || '')
    const streamKey = streamID || 'default'
    const nowMs = Number(payload.ts_unix_ms ?? Date.now())

    const flowState = flowStateRef.current[streamKey] || {
      seen: {},
      events: [],
    }
    for (const t of tracks) {
      const laneID = t?.lane_id
      const trackID = t?.track_id
      if (!laneID || trackID == null) continue
      if (localLaneIDs.size > 0 && !localLaneIDs.has(String(laneID))) continue
      const key = `${trackID}:${laneID}`
      if (!flowState.seen[key]) {
        flowState.seen[key] = nowMs
        flowState.events.push({ ts: nowMs, lane_id: String(laneID) })
      }
    }
    const cutoffMs = nowMs - laneFlowWindowSec * 1000
    flowState.events = flowState.events.filter((e) => e.ts >= cutoffMs)
    const pruneSeenBefore = nowMs - 10 * 60 * 1000
    Object.keys(flowState.seen).forEach((k) => {
      if (flowState.seen[k] < pruneSeenBefore) delete flowState.seen[k]
    })
    flowStateRef.current[streamKey] = flowState

    const perLane = {}
    if (localLaneIDs.size > 0) {
      for (const e of flowState.events) {
        if (localLaneIDs.has(e.lane_id)) {
          perLane[e.lane_id] = (perLane[e.lane_id] || 0) + 1
        }
      }
    }
    const totalPerMin = localLaneIDs.size > 0
      ? Object.values(perLane).reduce((acc, n) => acc + Number(n || 0), 0)
      : 0
    setLaneFlowStats({ totalPerMin, perLane })

    const nextIDMap = { ...overlayIDMapRef.current }
    let alphaIndex = Object.keys(nextIDMap).length
    const toAlpha = (idx) => {
      let n = idx
      let out = ''
      do {
        out = String.fromCharCode(65 + (n % 26)) + out
        n = Math.floor(n / 26) - 1
      } while (n >= 0)
      return out
    }
    const entities = tracks
      .filter((t) => t?.track_id != null)
      .map((t) => {
        const trackKey = String(t.track_id)
        if (!nextIDMap[trackKey]) {
          nextIDMap[trackKey] = toAlpha(alphaIndex)
          alphaIndex += 1
        }
        const detMatch = detections.find((d) => Number(d.track_id) === Number(t.track_id))
        const cls = String(detMatch?.class_name || t.class_name || 'unknown')
        const confidence = Number(detMatch?.confidence ?? t.confidence ?? 0)
        const speedPxS = Number(t.speed_px_s ?? 0)
        return {
          track_id: Number(t.track_id),
          overlay_id: nextIDMap[trackKey],
          class_name: cls,
          lane_id: t.lane_id ?? null,
          speed_px_s: speedPxS,
          confidence,
          is_moving: speedPxS >= 12.0,
        }
      })
      .sort((a, b) => a.overlay_id.localeCompare(b.overlay_id))
    overlayIDMapRef.current = nextIDMap
    setTrackedEntities(entities)

    setDetectStatus((prev) => ({
      ...prev,
      phase: 'ready',
      inProgress: false,
      detectorAvailable: true,
      consecutiveFailures: 0,
      retryAfterMs: 0,
      lastError: '',
      lastDurationMs: Number(payload.inference_ms ?? 0),
      detectionsCount: detections.length,
      tracksCount: tracks.length,
      queueLike: Boolean(metrics.queue_like),
      stoppedLike: Boolean(metrics.stopped_like),
      meanSmoothedSpeedPxS: Number(
        metrics.mean_smoothed_speed_px_s ?? metrics.mean_track_speed_px_s ?? 0,
      ),
    }))
  }, [focusCameraID, laneGeometryByCamera])

  const totalFeeds = useMemo(() => recommended.length, [recommended])
  const totalAlerts = useMemo(() => alerts.length, [alerts])

  async function fetchPipelineStatus() {
    const resp = await fetch(`${apiBase}/api/v1/pipeline/status`)
    if (!resp.ok) {
      throw new Error(`pipeline status endpoint failed (${resp.status})`)
    }
    const payload = await resp.json()
    setPipelineStatus(payload)
    return payload
  }

  async function loadDashboard() {
    setLoading(true)
    setError('')
    try {
      const [camsResp, planResp, alertsResp, statusResp, viewsResp] = await Promise.all([
        fetch(`${apiBase}/api/v1/cameras/recommended?count=10`),
        fetch(`${apiBase}/api/v1/analysis/plan`),
        fetch(`${apiBase}/api/v1/alerts?limit=100`),
        fetch(`${apiBase}/api/v1/pipeline/status`),
        fetch(`${apiBase}/api/v1/pipeline/cameras`),
      ])

      if (!camsResp.ok) {
        throw new Error(`camera endpoint failed (${camsResp.status})`)
      }
      if (!planResp.ok) {
        throw new Error(`analysis plan endpoint failed (${planResp.status})`)
      }
      if (!alertsResp.ok) {
        throw new Error(`alerts endpoint failed (${alertsResp.status})`)
      }
      if (!statusResp.ok) {
        throw new Error(`pipeline status endpoint failed (${statusResp.status})`)
      }
      if (!viewsResp.ok) {
        throw new Error(`pipeline camera view endpoint failed (${viewsResp.status})`)
      }

      const camsPayload = await camsResp.json()
      const planPayload = await planResp.json()
      const alertsPayload = await alertsResp.json()
      const statusPayload = await statusResp.json()
      const viewsPayload = await viewsResp.json()

      setRecommended(camsPayload.data || [])
      setAnalysisPlan(planPayload)
      setAlerts(alertsPayload.data || [])
      setPipelineStatus(statusPayload)
      setCameraViews(viewsPayload.data || [])

      if (!focusCameraID && viewsPayload.data?.length) {
        setFocusCameraID(viewsPayload.data[0].camera_id)
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : 'unknown error')
    } finally {
      setLoading(false)
    }
  }

  async function startPipeline() {
    setStarting(true)
    setError('')
    try {
      const resp = await fetch(`${apiBase}/api/v1/pipeline/start`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          camera_count: 10,
          sample_interval_sec: 3,
          buffer_seconds: 90,
          pre_event_seconds: 30,
          post_event_seconds: 45,
        }),
      })
      if (!resp.ok) {
        const payload = await resp.json().catch(() => ({}))
        throw new Error(payload.error || `pipeline start failed (${resp.status})`)
      }
      await loadDashboard()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'unknown error')
    } finally {
      setStarting(false)
    }
  }

  async function stopPipeline() {
    setStopping(true)
    setError('')
    try {
      const resp = await fetch(`${apiBase}/api/v1/pipeline/stop`, {
        method: 'POST',
      })
      if (!resp.ok) {
        const payload = await resp.json().catch(() => ({}))
        throw new Error(payload.error || `pipeline stop failed (${resp.status})`)
      }
      await loadDashboard()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'unknown error')
    } finally {
      setStopping(false)
    }
  }

  useEffect(() => {
    loadDashboard()
  }, [])

  useEffect(() => {
    saveLaneGeometryToStorage(laneGeometryByCamera)
  }, [laneGeometryByCamera])

  useEffect(() => {
    const id = setInterval(() => {
      Promise.all([
        fetch(`${apiBase}/api/v1/alerts?limit=100`)
          .then((r) => (r.ok ? r.json() : Promise.reject(new Error(`alerts refresh failed (${r.status})`)))),
        fetch(`${apiBase}/api/v1/pipeline/cameras`)
          .then((r) => (r.ok ? r.json() : Promise.reject(new Error(`camera view refresh failed (${r.status})`)))),
      ])
        .then(([alertsPayload, viewsPayload]) => {
          setAlerts(alertsPayload.data || [])
          setCameraViews(viewsPayload.data || [])
          if (!focusCameraID && viewsPayload.data?.length) {
            setFocusCameraID(viewsPayload.data[0].camera_id)
          }
        })
        .catch(() => {})

      fetchPipelineStatus().catch(() => {})
    }, 5000)
    return () => clearInterval(id)
  }, [focusCameraID])

  const focusedView = useMemo(
    () => cameraViews.find((v) => v.camera_id === focusCameraID) || null,
    [cameraViews, focusCameraID],
  )

  const focusLiveSrc = useMemo(() => {
    if (!focusCameraID) return ''
    return `${apiBase}/api/v1/pipeline/focus/snapshot?camera_id=${focusCameraID}&mode=live&_=${focusCacheBust}`
  }, [focusCameraID, focusCacheBust])

  const focusProcessedSrc = useMemo(() => {
    if (!focusCameraID) return ''
    return `${apiBase}/api/v1/pipeline/focus/snapshot?camera_id=${focusCameraID}&mode=processed&_=${focusCacheBust}`
  }, [focusCameraID, focusCacheBust])

  useEffect(() => {
    if (!focusCameraID) return undefined
    const id = window.setInterval(() => {
      setFocusCacheBust(Date.now())
    }, focusRefreshIntervalMs)
    return () => window.clearInterval(id)
  }, [focusCameraID])

  useEffect(() => {
    setDetectorMetrics(null)
    setLaneFlowStats({ totalPerMin: 0, perLane: {} })
    setTrackedEntities([])
    overlayIDMapRef.current = {}
    setLaneEditMode(false)
  }, [focusCameraID, focusedView?.stream_url])

  const currentCameraGeometry = useMemo(() => {
    if (!focusCameraID) return null
    return laneGeometryByCamera[String(focusCameraID)] || null
  }, [focusCameraID, laneGeometryByCamera])

  const handleLaneGeometryChange = useCallback(
    (geometry) => {
      if (!focusCameraID) return
      const key = String(focusCameraID)
      setLaneGeometryByCamera((prev) => {
        const next = { ...prev }
        if (!geometry || !Array.isArray(geometry.lanes) || geometry.lanes.length === 0) {
          delete next[key]
          return next
        }
        next[key] = {
          ...geometry,
          camera_id: focusCameraID,
          updated_at: Date.now(),
        }
        return next
      })
    },
    [focusCameraID],
  )

  const clearCurrentCameraGeometry = useCallback(() => {
    if (!focusCameraID) return
    const key = String(focusCameraID)
    setLaneGeometryByCamera((prev) => {
      const next = { ...prev }
      delete next[key]
      return next
    })
  }, [focusCameraID])

  return (
    <main className="app">
      <header className="appHeader">
        <div className="appHeaderText">
          <p className="appEyebrow">Live traffic · MVP</p>
          <h1>Incident Intelligence</h1>
          <p className="sub">
            Ingest feeds, detect events, score noise, emit alerts and evidence clips.
          </p>
        </div>
        <div className="buttonRow">
          <button type="button" onClick={loadDashboard} className="btn btnSecondary">
            Refresh
          </button>
          <button
            type="button"
            onClick={startPipeline}
            className="btn btnPrimary"
            disabled={starting || pipelineStatus?.running}
          >
            {starting ? 'Starting…' : 'Start pipeline'}
          </button>
          <button
            type="button"
            onClick={stopPipeline}
            className="btn btnDanger"
            disabled={stopping || !pipelineStatus?.running}
          >
            {stopping ? 'Stopping…' : 'Stop pipeline'}
          </button>
        </div>
      </header>

      <section className="metricsSection" aria-labelledby="overview-heading">
        <h2 id="overview-heading" className="sectionHeading">
          Overview
        </h2>
        <div className="metrics">
        <article className="metricCard">
          <h3 className="metricCardTitle">Pipeline</h3>
          <p className={`metricValue ${pipelineStatus?.running ? 'ok' : 'bad'}`}>
            {pipelineStatus?.running ? 'RUNNING' : 'STOPPED'}
          </p>
          <small>
            {pipelineStatus?.camera_count ?? 0} cameras, sample {pipelineStatus?.sample_interval || '...'}
          </small>
        </article>
        <article className="metricCard">
          <h3 className="metricCardTitle">Alert feed</h3>
          <p className="metricValue">{totalAlerts}</p>
          <small>structured events (not raw camera watching)</small>
        </article>
        <article className="metricCard">
          <h3 className="metricCardTitle">High-value cameras</h3>
          <p className="metricValue">{totalFeeds}</p>
          <small>v1 target: 5-10 high-value feeds</small>
        </article>
        <article className="metricCard">
          <h3 className="metricCardTitle">Latency (p95)</h3>
          <p className="metricValue">
            {analysisPlan?.latency_target_p95_sec
              ? `${analysisPlan.latency_target_p95_sec}s`
              : '...'}
          </p>
          <small>fast is the value prop</small>
        </article>
        <article className="metricCard">
          <h3 className="metricCardTitle">Inference FPS (plan)</h3>
          <p className="metricValue">{analysisPlan?.recommended_fps_per_cam || '...'}</p>
          <small>CPU-first, adaptive sampling</small>
        </article>
        <article className="metricCard">
          <h3 className="metricCardTitle">YOLO detector</h3>
          <p className="metricValue">{detectStatus.detectorAvailable ? 'Online' : 'Offline'}</p>
          <small>
            phase: {detectStatus.phase} | failures: {detectStatus.consecutiveFailures}
          </small>
        </article>
        <article className="metricCard">
          <h3 className="metricCardTitle">Focus detections</h3>
          <p className="metricValue">{detectStatus.detectionsCount}</p>
          <small>
            last run: {detectStatus.lastDurationMs}ms
            {detectStatus.retryAfterMs > 0 ? ` | retry in ${detectStatus.retryAfterMs}ms` : ''}
          </small>
        </article>
        <article className="metricCard">
          <h3 className="metricCardTitle">Tracked vehicles</h3>
          <p className="metricValue">{detectStatus.tracksCount}</p>
          <small>
            mean speed: {detectStatus.meanSmoothedSpeedPxS.toFixed(1)} px/s
            {' | '}
            queue_like: {detectStatus.queueLike ? 'yes' : 'no'}
            {' | '}
            stopped_like: {detectStatus.stoppedLike ? 'yes' : 'no'}
          </small>
        </article>
        </div>
      </section>

      <section className="signalSection" aria-labelledby="signals-heading">
        <h2 id="signals-heading" className="sectionHeading">
          Detection signals
        </h2>
        <div className="signalChips" role="list">
          {(analysisPlan?.alerts || []).map((alertName) => (
            <span key={alertName} className="signalChip" role="listitem">
              {alertName.replace(/_/g, ' ')}
            </span>
          ))}
        </div>
      </section>

      <section className="focusPanel" aria-labelledby="focus-heading">
        <div className="focusPanelHeader">
          <h2 id="focus-heading">Focus stream</h2>
          <p className="focusPanelLead">Pipeline snapshots plus live HLS decode with YOLO overlay.</p>
        </div>
        {cameraViews.length > 0 && (
          <div className="focusControls">
            <label className="focusControl">
              Camera
              <select
                value={focusCameraID ?? ''}
                onChange={(e) => setFocusCameraID(Number(e.target.value))}
              >
                {cameraViews.map((v) => (
                  <option key={v.camera_id} value={v.camera_id}>
                    {v.roadway || `Camera ${v.camera_id}`}
                  </option>
                ))}
              </select>
            </label>
            <label className="focusControl">
              Target FPS
              <input
                type="number"
                min={1}
                max={30}
                step={1}
                value={focusFPS}
                onChange={(e) =>
                  setFocusFPS(Math.min(30, Math.max(1, Number.parseInt(e.target.value, 10) || 1)))
                }
              />
            </label>
            <label className="focusControl">
              YOLO FPS
              <input
                type="number"
                min={1}
                max={30}
                step={1}
                value={detectorFPS}
                onChange={(e) =>
                  setDetectorFPS(Math.min(30, Math.max(1, Number.parseInt(e.target.value, 10) || 1)))
                }
              />
            </label>
            <label className="focusControl focusControlInline">
              Lane edit
              <button
                type="button"
                className="btn btnSecondary"
                onClick={() => setLaneEditMode((v) => !v)}
              >
                {laneEditMode ? 'Stop editing' : 'Draw lane polygons'}
              </button>
            </label>
          </div>
        )}
        {detectStatus.lastError && <p className="warn">Detector: {detectStatus.lastError}</p>}
        {focusCameraID ? (
          <div className="focusSnapshotsRow">
            <article className="focusCard focusCardSnapshot">
              <h3>Snapshot · live</h3>
              <div className="focusSnapshotFrame">
                <img src={focusLiveSrc} alt="Pipeline raw snapshot" loading="lazy" />
              </div>
            </article>
            <article className="focusCard focusCardSnapshot">
              <h3>Snapshot · processed</h3>
              <div className="focusSnapshotFrame">
                <img src={focusProcessedSrc} alt="Pipeline processed snapshot" loading="lazy" />
              </div>
            </article>
          </div>
        ) : (
          <p className="focusPlaceholder">Select a focus camera to poll snapshot JPEGs from the pipeline.</p>
        )}
        {focusedView ? (
          <>
            <h3 className="focusSubheading">Live decode</h3>
            {focusedView.stream_url ? (
              <FocusStreamFrames
                key={`${focusCameraID}-${focusedView.stream_url}`}
                streamUrl={focusedView.stream_url}
                fps={focusFPS}
                detectorFPS={detectorFPS}
                cameraID={focusCameraID}
                apiBase={apiBase}
                localLaneGeometry={currentCameraGeometry}
                laneEditMode={laneEditMode}
                onLaneGeometryChange={handleLaneGeometryChange}
                onDetectionMetrics={onDetectionMetrics}
              />
            ) : (
              <p className="focusPlaceholder">
                No HLS <code>stream_url</code> for this camera. Start the pipeline and pick a feed with video.
              </p>
            )}
            <div className="focusDetailCard">
              <dl className="focusMetaGrid">
                <div>
                  <dt>Roadway</dt>
                  <dd>{focusedView.roadway}</dd>
                </div>
                <div>
                  <dt>Location</dt>
                  <dd>{focusedView.location}</dd>
                </div>
              </dl>
              <div className="focusMetricsStrip">
                <div className="focusMetricPill">
                  <span className="focusMetricLabel">Vehicles</span>
                  <span className="focusMetricValue">{detectorMetrics?.metrics?.vehicle_count ?? 0}</span>
                </div>
                <div className="focusMetricPill">
                  <span className="focusMetricLabel">Moving ratio</span>
                  <span className="focusMetricValue">
                    {(((detectorMetrics?.metrics?.moving_ratio ?? 0) * 100)).toFixed(0)}%
                  </span>
                </div>
                <div className="focusMetricPill">
                  <span className="focusMetricLabel">Occupancy (YOLO)</span>
                  <span className="focusMetricValue">
                    {(detectorMetrics?.metrics?.occupancy_ratio ?? 0).toFixed(3)}
                  </span>
                </div>
                <div className="focusMetricPill">
                  <span className="focusMetricLabel">Speed</span>
                  <span className="focusMetricValue">
                    {Number(
                      detectorMetrics?.metrics?.mean_smoothed_speed_px_s ??
                      detectorMetrics?.metrics?.mean_track_speed_px_s ??
                      0,
                    ).toFixed(1)} px/s
                  </span>
                </div>
                <div className="focusMetricPill">
                  <span className="focusMetricLabel">Flow (last 60s)</span>
                  <span className="focusMetricValue">{laneFlowStats.totalPerMin}/min</span>
                </div>
                <div className="focusMetricPill">
                  <span className="focusMetricLabel">Lanes drawn</span>
                  <span className="focusMetricValue">{currentCameraGeometry?.lanes?.length ?? 0}</span>
                </div>
                <div className="focusMetricPill">
                  <span className="focusMetricLabel">Inference</span>
                  <span className="focusMetricValue">{Number(detectorMetrics?.inference_ms ?? 0).toFixed(0)} ms</span>
                </div>
                <div className="focusMetricPill">
                  <span className="focusMetricLabel">YOLO FPS cap</span>
                  <span className="focusMetricValue">{detectorFPS}</span>
                </div>
                <div className="focusMetricPill">
                  <span className="focusMetricLabel">Queue / stopped</span>
                  <span className="focusMetricValue">
                    {(detectorMetrics?.metrics?.queue_like ?? false) ? 'q' : '-'}
                    {' / '}
                    {(detectorMetrics?.metrics?.stopped_like ?? false) ? 's' : '-'}
                  </span>
                </div>
              </div>
              <div className="focusLaneEditorActions">
                <p className="focusLaneMeta">
                  {laneEditMode
                    ? 'Lane edit mode is ON: click processed canvas to place points, then add lane.'
                    : 'Lane edit mode is OFF.'}
                </p>
                <button type="button" className="btn btnSecondary" onClick={clearCurrentCameraGeometry}>
                  Clear lane polygons (this camera)
                </button>
                <p className="focusLaneMeta">
                  Polygons are saved locally in your browser for this camera.
                </p>
              </div>
              {detectorMetrics?.metrics?.counts_per_lane &&
                Object.keys(detectorMetrics.metrics.counts_per_lane).length > 0 && (
                  <p className="focusLaneMeta">
                    <strong>Lane counts:</strong>{' '}
                    {Object.entries(detectorMetrics.metrics.counts_per_lane)
                      .map(([laneID, count]) => `${laneID}:${count}`)
                      .join(' | ')}
                  </p>
                )}
              {Object.keys(laneFlowStats.perLane).length > 0 && (
                <p className="focusLaneMeta">
                  <strong>Lane flow (last 60s):</strong>{' '}
                  {Object.entries(laneFlowStats.perLane)
                    .map(([laneID, count]) => `${laneID}:${count}/min`)
                    .join(' | ')}
                </p>
              )}
              {focusedView.stream_url && (
                <a className="focusStreamLink" href={focusedView.stream_url} target="_blank" rel="noreferrer">
                  Open stream in new tab
                </a>
              )}
            </div>
          </>
        ) : (
          <p>Start the pipeline to see live processing for a selected feed.</p>
        )}
      </section>

      {loading && <p>Loading camera recommendations...</p>}
      {error && <p className="error">Error: {error}</p>}

      <section className="alertsSection" aria-labelledby="alerts-heading">
        <h2 id="alerts-heading" className="sectionHeading">
          Incident alerts
        </h2>
        <div className="cameraGrid">
          {alerts.map((alert) => (
            <article key={alert.id} className="cameraCard">
              <div className="cameraBody">
                <h3>{alert.event_type}</h3>
                <p>{alert.location}</p>
                <p>
                  <strong>When:</strong> {new Date(alert.timestamp).toLocaleString()}
                </p>
                <p>
                  <strong>Confidence:</strong> {(alert.confidence * 100).toFixed(1)}%
                </p>
                <p>
                  <strong>Reason:</strong> {alert.reason}
                </p>
                <div className="links">
                  {alert.before_image_url && (
                    <a href={`${apiBase}${alert.before_image_url}`} target="_blank" rel="noreferrer">
                      Before snapshot
                    </a>
                  )}
                  {alert.after_image_url && (
                    <a href={`${apiBase}${alert.after_image_url}`} target="_blank" rel="noreferrer">
                      After snapshot
                    </a>
                  )}
                  {alert.clip_url && (
                    <a href={`${apiBase}${alert.clip_url}`} target="_blank" rel="noreferrer">
                      Event clip
                    </a>
                  )}
                </div>
              </div>
            </article>
          ))}
          {!alerts.length && (
            <p className="emptyState">No alerts yet. Run the pipeline and wait for detected events.</p>
          )}
        </div>
      </section>

      <section className="recommendedSection" aria-labelledby="recommended-heading">
        <h2 id="recommended-heading" className="sectionHeading">
          Recommended cameras
        </h2>
        <div className="cameraGrid">
        {recommended.map((cam) => {
          const feed = cam.images?.[0]
          return (
            <article key={cam.id} className="cameraCard">
              <img
                src={buildImageURL(feed?.imageUrl)}
                alt={cam.location || cam.roadway || `Camera ${cam.id}`}
                loading="lazy"
              />
              <div className="cameraBody">
                <h3>{cam.roadway || `Camera ${cam.id}`}</h3>
                <p>{cam.location}</p>
                <p>
                  <strong>Direction:</strong> {cam.direction || 'Unknown'}
                </p>
                <p>
                  <strong>Score:</strong> {cam.score} ({cam.why})
                </p>
                <div className="links">
                  <a href={feed?.videoUrl} target="_blank" rel="noreferrer">
                    Open live stream
                  </a>
                  <a href={buildImageURL(feed?.imageUrl)} target="_blank" rel="noreferrer">
                    Open snapshot
                  </a>
                </div>
              </div>
            </article>
          )
        })}
        </div>
      </section>
    </main>
  )
}

export default App
