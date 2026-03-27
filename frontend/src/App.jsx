import { useEffect, useMemo, useState } from 'react'
import './App.css'

const apiBase = import.meta.env.VITE_API_BASE_URL || 'http://localhost:8080'

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
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [starting, setStarting] = useState(false)
  const [stopping, setStopping] = useState(false)
  const [focusFPS, setFocusFPS] = useState(30)

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
        <div className="focusHeader">
          <h2>Focused Live Processing</h2>
          <div className="focusControls">
            <select
              value={focusCameraID ?? ''}
              onChange={(e) => setFocusCameraID(Number(e.target.value))}
              disabled={!cameraViews.length}
            >
              {!cameraViews.length && <option value="">No active camera views</option>}
              {cameraViews.map((v) => (
                <option key={v.camera_id} value={v.camera_id}>
                  {v.roadway || `Camera ${v.camera_id}`} - {v.location || 'Unknown location'}
                </option>
              ))}
            </select>
            <label className="fpsLabel">
              FPS
              <input
                type="number"
                min="1"
                max="30"
                value={focusFPS}
                onChange={(e) => {
                  const next = Number(e.target.value)
                  if (Number.isNaN(next)) return
                  setFocusFPS(Math.max(1, Math.min(30, next)))
                }}
              />
            </label>
          </div>
        </div>
        {focusedView ? (
          <>
            <div className="focusGrid">
              <div className="focusCard">
                <h3>Live frame</h3>
                {focusedView.live_image_url ? (
                  <img
                    src={`${apiBase}/api/v1/focus/stream?camera_id=${focusedView.camera_id}&mode=raw&fps=${focusFPS}`}
                    alt="live camera frame"
                  />
                ) : (
                  <p>No frame yet</p>
                )}
              </div>
              <div className="focusCard">
                <h3>Processed frame</h3>
                {focusedView.processed_image_url ? (
                  <img
                    src={`${apiBase}/api/v1/focus/stream?camera_id=${focusedView.camera_id}&mode=processed&fps=${focusFPS}`}
                    alt="processed camera frame"
                  />
                ) : (
                  <p>No processed frame yet</p>
                )}
              </div>
            </div>
            <p>
              <strong>Camera:</strong> {focusedView.roadway} | <strong>Location:</strong>{' '}
              {focusedView.location}
            </p>
            <p>
              <strong>Motion:</strong> {focusedView.motion?.toFixed(4) ?? '0.0000'} |{' '}
              <strong>Occupancy:</strong> {focusedView.occupancy?.toFixed(4) ?? '0.0000'} |{' '}
              <strong>Failures:</strong> {focusedView.failures}
            </p>
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
