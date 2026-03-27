import { useCallback, useEffect, useMemo, useState } from 'react'
import './App.css'
import { FocusStreamFrames } from './FocusStreamFrames.jsx'

const apiBase = import.meta.env.VITE_API_BASE_URL || 'http://localhost:8080'
const focusRefreshIntervalMs = 2000
const detectLoopIntervalMs = 1200

function buildImageURL(path) {
  if (!path) return ''
  if (path.startsWith('http://') || path.startsWith('https://')) return path
  return `https://511ny.org${path}`
}

function sleep(ms) {
  return new Promise((resolve) => window.setTimeout(resolve, ms))
}

function App() {
  const [recommended, setRecommended] = useState([])
  const [alerts, setAlerts] = useState([])
  const [cameraViews, setCameraViews] = useState([])
  const [focusCameraID, setFocusCameraID] = useState(null)
  const [pipelineStatus, setPipelineStatus] = useState(null)
  const [analysisPlan, setAnalysisPlan] = useState(null)
  const [pipelineSummary, setPipelineSummary] = useState(null)
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
    laneCounts: {},
  })
  const [focusCacheBust, setFocusCacheBust] = useState(Date.now())
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [starting, setStarting] = useState(false)
  const [stopping, setStopping] = useState(false)
  const [focusFPS, setFocusFPS] = useState(30)
  const [detectorFPS, setDetectorFPS] = useState(2)
  /** Motion/occupancy from client-side frame diff on the HLS decode (same heuristics as API). */
  const [streamClientMetrics, setStreamClientMetrics] = useState(null)
  /** Detector-side metrics from YOLO sidecar via backend focus proxy. */
  const [detectorMetrics, setDetectorMetrics] = useState(null)

  const onStreamFrameMetrics = useCallback((m) => {
    setStreamClientMetrics(m)
  }, [])

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

  useEffect(() => {
    setStreamClientMetrics(null)
    setDetectorMetrics(null)
  }, [focusCameraID, focusedView?.stream_url])

  useEffect(() => {
    if (!pipelineSummary?.ok) return undefined

    let stopped = false
    let inFlight = false

    async function runDetectWorkflowLoop() {
      while (!stopped) {
        if (inFlight) {
          await sleep(150)
          continue
        }

        inFlight = true
        try {
          const detectResp = await fetch(
            `${apiBase}/api/v1/pipeline/focus/detect?imgsz=640&conf=0.25`,
            { method: 'POST' },
          )
          if (!detectResp.ok) {
            setDetectStatus((prev) => ({
              ...prev,
              phase: 'error',
              inProgress: false,
              detectorAvailable: false,
              consecutiveFailures: prev.consecutiveFailures + 1,
              lastError: `detect endpoint failed (${detectResp.status})`,
            }))
            await sleep(2000)
            continue
          }

          const payload = await detectResp.json()
          const workflow = payload?.workflow || {}
          const detections = payload?.detector?.detections
          const tracks = payload?.detector?.tracks
          const metrics = payload?.detector?.metrics || {}
          const detectionsCount = Array.isArray(detections)
            ? detections.length
            : Array.isArray(payload?.detector?.boxes)
              ? payload.detector.boxes.length
              : 0
          const tracksCount = Array.isArray(tracks) ? tracks.length : 0

          setDetectStatus({
            phase: workflow.phase || (payload.ok ? 'ready' : 'cooldown'),
            inProgress: Boolean(workflow.in_progress),
            detectorAvailable: Boolean(payload.detector_available),
            consecutiveFailures: Number(workflow.consecutive_failures || 0),
            retryAfterMs: Number(workflow.retry_after_ms || 0),
            lastError: payload.error || workflow.last_error || '',
            lastDurationMs: Number(workflow.last_duration_ms || 0),
            detectionsCount,
            tracksCount,
            queueLike: Boolean(metrics.queue_like),
            stoppedLike: Boolean(metrics.stopped_like),
            meanSmoothedSpeedPxS: Number(metrics.mean_smoothed_speed_px_s || 0),
            laneCounts:
              metrics.counts_per_lane && typeof metrics.counts_per_lane === 'object'
                ? metrics.counts_per_lane
                : {},
          })

          const cooldown = Number(workflow.retry_after_ms || 0)
          if (cooldown > 0) {
            await sleep(Math.min(Math.max(cooldown, 250), 5000))
          } else {
            await sleep(detectLoopIntervalMs)
          }
        } catch (err) {
          setDetectStatus((prev) => ({
            ...prev,
            phase: 'error',
            inProgress: false,
            detectorAvailable: false,
            consecutiveFailures: prev.consecutiveFailures + 1,
            lastError: err instanceof Error ? err.message : 'detect request failed',
          }))
          await sleep(2000)
        } finally {
          inFlight = false
        }
      }
    }

    runDetectWorkflowLoop()
    return () => {
      stopped = true
    }
  }, [pipelineSummary?.ok])

  return (
    <main className="app">
      <header className="header">
        <div>
          <h1>Incident Intelligence MVP</h1>
          <p className="sub">
            Ingest feeds → detect events → score noise → emit alerts + evidence clips.
          </p>
        </div>
        <div className="buttonRow">
          <button onClick={loadDashboard} className="refreshBtn">
            Refresh
          </button>
          <button onClick={startPipeline} className="refreshBtn" disabled={starting || pipelineStatus?.running}>
            {starting ? 'Starting...' : 'Start pipeline'}
          </button>
          <button onClick={stopPipeline} className="refreshBtn danger" disabled={stopping || !pipelineStatus?.running}>
            {stopping ? 'Stopping...' : 'Stop pipeline'}
          </button>
        </div>
      </header>

      <section className="metrics">
        <article className="metricCard">
          <h2>Pipeline</h2>
          <p className={`metricValue ${pipelineStatus?.running ? 'ok' : 'bad'}`}>
            {pipelineStatus?.running ? 'RUNNING' : 'STOPPED'}
          </p>
          <small>
            {pipelineStatus?.camera_count ?? 0} cameras, sample {pipelineStatus?.sample_interval || '...'}
          </small>
        </article>
        <article className="metricCard">
          <h2>Alert Feed</h2>
          <p className="metricValue">{totalAlerts}</p>
          <small>structured events (not raw camera watching)</small>
        </article>
        <article className="metricCard">
          <h2>High-Value Cameras</h2>
          <p className="metricValue">{totalFeeds}</p>
          <small>v1 target: 5-10 high-value feeds</small>
        </article>
        <article className="metricCard">
          <h2>Latency Target (p95)</h2>
          <p className="metricValue">
            {analysisPlan?.latency_target_p95_sec
              ? `${analysisPlan.latency_target_p95_sec}s`
              : '...'}
          </p>
          <small>fast is the value prop</small>
        </article>
        <article className="metricCard">
          <h2>Recommended Inference FPS</h2>
          <p className="metricValue">{analysisPlan?.recommended_fps_per_cam || '...'}</p>
          <small>CPU-first, adaptive sampling</small>
        </article>
        <article className="metricCard">
          <h2>YOLO Detector</h2>
          <p className="metricValue">{detectStatus.detectorAvailable ? 'Online' : 'Offline'}</p>
          <small>
            phase: {detectStatus.phase} | failures: {detectStatus.consecutiveFailures}
          </small>
        </article>
        <article className="metricCard">
          <h2>Focus Detections</h2>
          <p className="metricValue">{detectStatus.detectionsCount}</p>
          <small>
            last run: {detectStatus.lastDurationMs}ms
            {detectStatus.retryAfterMs > 0 ? ` | retry in ${detectStatus.retryAfterMs}ms` : ''}
          </small>
        </article>
        <article className="metricCard">
          <h2>Tracked Vehicles</h2>
          <p className="metricValue">{detectStatus.tracksCount}</p>
          <small>
            mean speed: {detectStatus.meanSmoothedSpeedPxS.toFixed(1)} px/s
            {' | '}
            queue_like: {detectStatus.queueLike ? 'yes' : 'no'}
            {' | '}
            stopped_like: {detectStatus.stoppedLike ? 'yes' : 'no'}
          </small>
        </article>
      </section>

      <section className="alerts">
        <h2>Detection Signals</h2>
        <ul>
          {(analysisPlan?.alerts || []).map((alertName) => (
            <li key={alertName}>{alertName}</li>
          ))}
        </ul>
      </section>

      <section className="focusPanel">
        <h2>Focus Stream</h2>
        {detectStatus.lastError && <p className="warn">Detector: {detectStatus.lastError}</p>}
        {Object.keys(detectStatus.laneCounts || {}).length > 0 && (
          <p className="focusLaneMeta">
            Lane counts:{' '}
            {Object.entries(detectStatus.laneCounts)
              .map(([lane, count]) => `${lane}=${count}`)
              .join(' | ')}
          </p>
        )}
        <div className="focusGrid">
          <article className="focusCard">
            <h3>Raw Snapshot (mode=live)</h3>
            <img src={focusLiveSrc} alt="Focus stream raw snapshot" loading="lazy" />
          </article>
          <article className="focusCard">
            <h3>Overlay Preview (mode=processed)</h3>
            <img src={focusProcessedSrc} alt="Focus stream processed overlay" loading="lazy" />
          </article>
        </div>
        {focusedView ? (
          <>
            {focusedView.stream_url ? (
              <FocusStreamFrames
                key={`${focusCameraID}-${focusedView.stream_url}`}
                streamUrl={focusedView.stream_url}
                fps={focusFPS}
                detectorFPS={detectorFPS}
                cameraID={focusCameraID}
                apiBase={apiBase}
                onFrameMetrics={onStreamFrameMetrics}
                onDetectionMetrics={setDetectorMetrics}
              />
            ) : (
              <p className="focusPlaceholder">
                No HLS <code>stream_url</code> for this camera. Start the pipeline and pick a feed with video.
              </p>
            )}
            <p>
              <strong>Camera:</strong> {focusedView.roadway} | <strong>Location:</strong>{' '}
              {focusedView.location}
            </p>
            <p>
              <strong>Pipeline (snapshots):</strong> motion {focusedView.motion?.toFixed(4) ?? '0.0000'} | occupancy{' '}
              {focusedView.occupancy?.toFixed(4) ?? '0.0000'} | failures {focusedView.failures}
            </p>
            {streamClientMetrics && (
              <p className="focusMeta">
                <strong>Stream frames (client, {focusFPS} fps target):</strong> motion{' '}
                {streamClientMetrics.motion.toFixed(4)} | occupancy {streamClientMetrics.occupancy.toFixed(4)}
              </p>
            )}
            {detectorMetrics?.metrics && (
              <p className="focusMeta">
                <strong>YOLO (server, {detectorFPS} fps target):</strong> vehicles{' '}
                {detectorMetrics.metrics.vehicle_count ?? 0} | moving{' '}
                {detectorMetrics.metrics.moving_vehicle_count ?? 0} | occupancy{' '}
                {(detectorMetrics.metrics.occupancy_ratio ?? 0).toFixed(4)} | infer{' '}
                {Number(detectorMetrics.inference_ms ?? 0).toFixed(1)}ms
              </p>
            )}
            {focusedView.stream_url && (
              <p>
                <a href={focusedView.stream_url} target="_blank" rel="noreferrer">
                  Open upstream stream URL
                </a>
              </p>
            )}
          </>
        ) : (
          <p>Start the pipeline to see live processing for a selected feed.</p>
        )}
      </section>

      {loading && <p>Loading camera recommendations...</p>}
      {error && <p className="error">Error: {error}</p>}

      <section>
        <h2>Incident Alerts</h2>
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
          {!alerts.length && <p>No alerts yet. Start pipeline and wait for detected events.</p>}
        </div>
      </section>

      <section className="cameraGrid">
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
      </section>
    </main>
  )
}

export default App
