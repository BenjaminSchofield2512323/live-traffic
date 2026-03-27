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
  const [analysisPlan, setAnalysisPlan] = useState(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')

  const totalFeeds = useMemo(() => recommended.length, [recommended])

  async function loadDashboard() {
    setLoading(true)
    setError('')
    try {
      const [camsResp, planResp] = await Promise.all([
        fetch(`${apiBase}/api/v1/cameras/recommended?count=10`),
        fetch(`${apiBase}/api/v1/analysis/plan`),
      ])

      if (!camsResp.ok) {
        throw new Error(`camera endpoint failed (${camsResp.status})`)
      }
      if (!planResp.ok) {
        throw new Error(`analysis plan endpoint failed (${planResp.status})`)
      }

      const camsPayload = await camsResp.json()
      const planPayload = await planResp.json()

      setRecommended(camsPayload.data || [])
      setAnalysisPlan(planPayload)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'unknown error')
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    loadDashboard()
  }, [])

  return (
    <main className="app">
      <header className="header">
        <div>
          <h1>Live Traffic MVP</h1>
          <p className="sub">
            Go backend + endpoint-based 511 ingestion + fast alert-first design.
          </p>
        </div>
        <button onClick={loadDashboard} className="refreshBtn">
          Refresh feeds
        </button>
      </header>

      <section className="metrics">
        <article className="metricCard">
          <h2>Recommended Cameras</h2>
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
      </section>

      <section className="alerts">
        <h2>Planned Alert Types</h2>
        <ul>
          {(analysisPlan?.alerts || []).map((alertName) => (
            <li key={alertName}>{alertName}</li>
          ))}
        </ul>
      </section>

      {loading && <p>Loading camera recommendations...</p>}
      {error && <p className="error">Error: {error}</p>}

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
                  <a
                    href={buildImageURL(feed?.imageUrl)}
                    target="_blank"
                    rel="noreferrer"
                  >
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
