import { useEffect, useMemo, useRef, useState } from 'react'
import Hls from 'hls.js'
import { deriveMetrics, drawProcessedOverlay, imageDataToGray } from './focusFrameMetrics.js'

const MW = 64
const MH = 36

/** Flat detector JSON or legacy `{ detector: { ... } }` wrapper from older proxies. */
function normalizeDetectorPayload(p) {
  if (!p || typeof p !== 'object') {
    return { image: null, detections: [] }
  }
  const inner = p.detector && typeof p.detector === 'object' ? p.detector : p
  const image = inner.image ?? p.image
  const detections = Array.isArray(inner.detections)
    ? inner.detections
    : Array.isArray(p.detections)
      ? p.detections
      : []
  return { image, detections }
}

function trackNumberToLabel(trackID) {
  const n = Number(trackID)
  if (!Number.isFinite(n) || n < 1) return String(trackID ?? '?')
  let x = Math.floor(n)
  let label = ''
  while (x > 0) {
    const rem = (x - 1) % 26
    label = String.fromCharCode(65 + rem) + label
    x = Math.floor((x - 1) / 26)
  }
  return label || String(trackID)
}

function drawLanePolygons(ctx, payload, dw, dh) {
  const geometry = payload?.geometry ?? payload?.detector?.geometry
  if (!geometry || !Array.isArray(geometry.lanes) || geometry.lanes.length === 0) {
    return
  }
  const detW = Number(payload?.image?.width || payload?.detector?.image?.width || 0) || dw
  const detH = Number(payload?.image?.height || payload?.detector?.image?.height || 0) || dh
  const sx = detW > 0 ? dw / detW : 1
  const sy = detH > 0 ? dh / detH : 1

  ctx.save()
  ctx.lineWidth = 2
  ctx.strokeStyle = 'rgba(80,180,255,0.95)'
  ctx.fillStyle = 'rgba(80,180,255,0.10)'
  ctx.font = '12px sans-serif'

  geometry.lanes.forEach((lane) => {
    const poly = Array.isArray(lane?.polygon) ? lane.polygon : []
    if (poly.length < 3) return

    ctx.beginPath()
    poly.forEach((pt, i) => {
      if (!Array.isArray(pt) || pt.length < 2) return
      const x = Number(pt[0]) * sx
      const y = Number(pt[1]) * sy
      if (i === 0) ctx.moveTo(x, y)
      else ctx.lineTo(x, y)
    })
    ctx.closePath()
    ctx.fill()
    ctx.stroke()

    // Label near centroid for lane readability.
    const validPts = poly.filter((pt) => Array.isArray(pt) && pt.length >= 2)
    if (validPts.length > 0) {
      const cx = validPts.reduce((acc, pt) => acc + Number(pt[0]), 0) / validPts.length
      const cy = validPts.reduce((acc, pt) => acc + Number(pt[1]), 0) / validPts.length
      const laneID = String(lane?.lane_id ?? lane?.id ?? 'lane')
      ctx.fillStyle = 'rgba(80,180,255,0.95)'
      ctx.fillText(laneID, cx * sx + 2, cy * sy - 2)
      ctx.fillStyle = 'rgba(80,180,255,0.10)'
    }
  })
  ctx.restore()
}

function normalizeLocalLaneGeometry(localLaneGeometry) {
  if (!localLaneGeometry || !Array.isArray(localLaneGeometry.lanes)) return null
  const lanes = localLaneGeometry.lanes
    .map((lane, idx) => {
      const laneID = String(lane?.lane_id || lane?.id || `lane_${idx + 1}`).trim()
      const poly = Array.isArray(lane?.polygon)
        ? lane.polygon
            .filter((pt) => Array.isArray(pt) && pt.length >= 2)
            .map((pt) => [Number(pt[0]), Number(pt[1])])
        : []
      if (!laneID || poly.length < 3) return null
      return { lane_id: laneID, polygon: poly }
    })
    .filter(Boolean)
  if (!lanes.length) return null
  return { lanes }
}

function buildRoadPolygonFromLanes(lanes) {
  const points = []
  lanes.forEach((lane) => {
    lane.polygon.forEach((pt) => points.push(pt))
  })
  if (points.length < 3) return null
  const xs = points.map((pt) => Number(pt[0]))
  const ys = points.map((pt) => Number(pt[1]))
  const minX = Math.min(...xs)
  const maxX = Math.max(...xs)
  const minY = Math.min(...ys)
  const maxY = Math.max(...ys)
  return [
    [minX, minY],
    [maxX, minY],
    [maxX, maxY],
    [minX, maxY],
  ]
}

function absolutize511StreamUrl(u) {
  if (!u) return ''
  const s = String(u).trim()
  if (s.startsWith('http://') || s.startsWith('https://')) return s
  if (s.startsWith('//')) return `https:${s}`
  return `https://511ny.org${s.startsWith('/') ? s : `/${s}`}`
}

/**
 * Hidden HLS video decodes the stream; two canvases show raw vs processed (overlay)
 * at the requested FPS — no visible video player.
 */
export function FocusStreamFrames({
  streamUrl,
  fps,
  detectorFPS = 2,
  localLaneGeometry = null,
  laneEditMode = false,
  onLaneGeometryChange,
  cameraID,
  apiBase,
  onFrameMetrics,
  onDetectionMetrics,
}) {
  const videoRef = useRef(null)
  const hlsRef = useRef(null)
  const rawRef = useRef(null)
  const procRef = useRef(null)
  const metricsCanvasRef = useRef(null)
  const prevGrayRef = useRef(null)
  const rafRef = useRef(0)
  const lastFrameRef = useRef(0)
  const metricsCbRef = useRef(onFrameMetrics)
  const detectionCbRef = useRef(onDetectionMetrics)
  const detectInflightRef = useRef(false)
  const lastDetectAtRef = useRef(0)
  const latestDetectionRef = useRef(null)
  const latestFrameSizeRef = useRef({ width: 0, height: 0 })
  const draftPolyRef = useRef([])
  const [laneEditError, setLaneEditError] = useState('')
  const [streamReady, setStreamReady] = useState(false)
  const [hlsError, setHlsError] = useState(null)

  const normalizedLocalGeometry = useMemo(
    () => normalizeLocalLaneGeometry(localLaneGeometry),
    [localLaneGeometry],
  )

  useEffect(() => {
    metricsCbRef.current = onFrameMetrics
  }, [onFrameMetrics])

  useEffect(() => {
    detectionCbRef.current = onDetectionMetrics
  }, [onDetectionMetrics])

  useEffect(() => {
    latestDetectionRef.current = null
    detectInflightRef.current = false
    lastDetectAtRef.current = 0
    detectionCbRef.current?.(null)
    draftPolyRef.current = []
  }, [streamUrl, cameraID, localLaneGeometry])

  const handleCanvasClick = (evt) => {
    if (!laneEditMode) return
    const proc = procRef.current
    if (!proc) return
    const rect = proc.getBoundingClientRect()
    if (rect.width <= 0 || rect.height <= 0) return
    const x = ((evt.clientX - rect.left) / rect.width) * proc.width
    const y = ((evt.clientY - rect.top) / rect.height) * proc.height
    const px = Math.max(0, Math.min(proc.width - 1, Math.round(x)))
    const py = Math.max(0, Math.min(proc.height - 1, Math.round(y)))
    draftPolyRef.current = [...draftPolyRef.current, [px, py]]
    setLaneEditError('')
  }

  const commitDraftLane = () => {
    if (!laneEditMode) return
    const draft = draftPolyRef.current
    if (!draft || draft.length < 3) {
      setLaneEditError('Need at least 3 points to create a lane.')
      return
    }
    const base = normalizedLocalGeometry?.lanes || []
    const laneID = `lane_${base.length + 1}`
    const nextLanes = [...base, { lane_id: laneID, polygon: draft }]
    const roadPolygon = buildRoadPolygonFromLanes(nextLanes)
    onLaneGeometryChange?.({
      lanes: nextLanes,
      road_polygon: roadPolygon,
      detector_input_resolution: latestFrameSizeRef.current,
    })
    draftPolyRef.current = []
    setLaneEditError('')
  }

  const clearDraftLane = () => {
    draftPolyRef.current = []
    setLaneEditError('')
  }

  const removeLastLane = () => {
    const base = normalizedLocalGeometry?.lanes || []
    if (!base.length) return
    const nextLanes = base.slice(0, -1)
    const roadPolygon = nextLanes.length ? buildRoadPolygonFromLanes(nextLanes) : null
    onLaneGeometryChange?.(
      nextLanes.length
        ? {
            lanes: nextLanes,
            road_polygon: roadPolygon,
            detector_input_resolution: latestFrameSizeRef.current,
          }
        : null,
    )
  }

  useEffect(() => {
    const video = videoRef.current
    const url = absolutize511StreamUrl(streamUrl)
    if (!url || !video) return undefined

    const cleanupVideo = () => {
      video.pause()
      video.removeAttribute('src')
      video.load()
    }

    if (Hls.isSupported()) {
      const hls = new Hls({
        enableWorker: true,
        lowLatencyMode: true,
      })
      hlsRef.current = hls
      hls.loadSource(url)
      hls.attachMedia(video)
      hls.on(Hls.Events.MANIFEST_PARSED, () => {
        video.play().catch(() => {})
        setStreamReady(true)
      })
      hls.on(Hls.Events.ERROR, (_, data) => {
        if (!data.fatal) return
        if (data.type === Hls.ErrorTypes.NETWORK_ERROR) {
          hls.startLoad()
          return
        }
        if (data.type === Hls.ErrorTypes.MEDIA_ERROR) {
          hls.recoverMediaError()
          return
        }
        setHlsError(data.details || String(data.type))
        setStreamReady(false)
        hls.destroy()
        hlsRef.current = null
      })
      return () => {
        if (hlsRef.current) {
          hlsRef.current.destroy()
          hlsRef.current = null
        }
        cleanupVideo()
        queueMicrotask(() => setStreamReady(false))
      }
    }

    if (video.canPlayType('application/vnd.apple.mpegurl')) {
      video.src = url
      const onLoaded = () => setStreamReady(true)
      video.addEventListener('loadeddata', onLoaded)
      video.play().catch(() => {})
      return () => {
        video.removeEventListener('loadeddata', onLoaded)
        cleanupVideo()
      }
    }

    queueMicrotask(() => setHlsError('HLS not supported'))
    return undefined
  }, [streamUrl])

  useEffect(() => {
    const video = videoRef.current
    const raw = rawRef.current
    const proc = procRef.current
    const metricsCanvas = metricsCanvasRef.current
    if (!video || !raw || !proc || !metricsCanvas) return undefined

    const ctxM = metricsCanvas.getContext('2d', { willReadFrequently: true })
    const intervalMs = 1000 / Math.max(1, Math.min(30, fps))
    const boundedDetectorFPS = Math.max(1, Math.min(30, Number(detectorFPS) || 2))
    const detectorIntervalMs = 1000 / boundedDetectorFPS

    const tick = (now) => {
      if (video.readyState < 2 || !video.videoWidth) {
        rafRef.current = requestAnimationFrame(tick)
        return
      }

      if (now - lastFrameRef.current < intervalMs) {
        rafRef.current = requestAnimationFrame(tick)
        return
      }
      lastFrameRef.current = now

      const vw = video.videoWidth
      const vh = video.videoHeight
      const maxW = 640
      const scale = Math.min(1, maxW / vw)
      const dw = Math.floor(vw * scale)
      const dh = Math.floor(vh * scale)
      latestFrameSizeRef.current = { width: dw, height: dh }

      raw.width = dw
      raw.height = dh
      proc.width = dw
      proc.height = dh
      metricsCanvas.width = MW
      metricsCanvas.height = MH

      const rawCtx = raw.getContext('2d')
      const procCtx = proc.getContext('2d')

      rawCtx.drawImage(video, 0, 0, dw, dh)
      procCtx.drawImage(video, 0, 0, dw, dh)

      ctxM.drawImage(video, 0, 0, MW, MH)
      const imgData = ctxM.getImageData(0, 0, MW, MH)
      const gray = imageDataToGray(imgData)
      const prev = prevGrayRef.current
      const { motion, occupancy } = deriveMetrics(prev, gray, MW, MH)
      prevGrayRef.current = gray

      drawProcessedOverlay(procCtx, dw, dh, motion, occupancy)
      const rawPayload = latestDetectionRef.current
      const { image: detImage, detections: detList } = normalizeDetectorPayload(rawPayload)
      drawLanePolygons(procCtx, rawPayload, dw, dh)
      if (laneEditMode) {
        const localLanes = normalizedLocalGeometry?.lanes || []
        procCtx.save()
        procCtx.lineWidth = 2
        procCtx.strokeStyle = 'rgba(255, 195, 0, 0.95)'
        procCtx.fillStyle = 'rgba(255, 195, 0, 0.1)'
        procCtx.font = '12px sans-serif'
        localLanes.forEach((lane) => {
          const poly = lane.polygon || []
          if (poly.length < 3) return
          procCtx.beginPath()
          poly.forEach((pt, i) => {
            const x = Number(pt[0])
            const y = Number(pt[1])
            if (i === 0) procCtx.moveTo(x, y)
            else procCtx.lineTo(x, y)
          })
          procCtx.closePath()
          procCtx.fill()
          procCtx.stroke()
          const cx = poly.reduce((acc, pt) => acc + Number(pt[0]), 0) / poly.length
          const cy = poly.reduce((acc, pt) => acc + Number(pt[1]), 0) / poly.length
          procCtx.fillStyle = 'rgba(255, 195, 0, 0.95)'
          procCtx.fillText(String(lane.lane_id), cx + 2, cy - 2)
          procCtx.fillStyle = 'rgba(255, 195, 0, 0.1)'
        })

        const draft = draftPolyRef.current
        if (draft.length > 0) {
          procCtx.strokeStyle = 'rgba(255, 90, 90, 0.95)'
          procCtx.fillStyle = 'rgba(255, 90, 90, 0.18)'
          procCtx.beginPath()
          draft.forEach((pt, i) => {
            const x = Number(pt[0])
            const y = Number(pt[1])
            if (i === 0) procCtx.moveTo(x, y)
            else procCtx.lineTo(x, y)
            procCtx.fillRect(x - 2, y - 2, 4, 4)
          })
          if (draft.length >= 3) {
            procCtx.closePath()
            procCtx.fill()
          }
          procCtx.stroke()
        }
        procCtx.restore()
      }
      if (Array.isArray(detList) && detList.length > 0) {
        const detW = Number(detImage?.width || 0) || dw
        const detH = Number(detImage?.height || 0) || dh
        const sx = detW > 0 ? dw / detW : 1
        const sy = detH > 0 ? dh / detH : 1

        procCtx.lineWidth = 2
        procCtx.strokeStyle = 'rgba(45,210,80,0.95)'
        procCtx.font = '12px sans-serif'
        procCtx.fillStyle = 'rgba(45,210,80,0.95)'
        detList.forEach((d) => {
          if (!Array.isArray(d.bbox) || d.bbox.length < 4) return
          const [x1, y1, x2, y2] = d.bbox
          const bx = x1 * sx
          const by = y1 * sy
          const bw = Math.max(1, (x2 - x1) * sx)
          const bh = Math.max(1, (y2 - y1) * sy)
          procCtx.strokeRect(bx, by, bw, bh)
          const tag = trackNumberToLabel(d.track_id)
          procCtx.fillText(tag, bx + 2, Math.max(12, by - 3))
        })
      }
      metricsCbRef.current?.({ motion, occupancy })

      const shouldDetect =
        cameraID &&
        !detectInflightRef.current &&
        now - lastDetectAtRef.current >= detectorIntervalMs
      if (shouldDetect) {
        detectInflightRef.current = true
        lastDetectAtRef.current = now
        raw.toBlob(async (blob) => {
          if (!blob) {
            detectInflightRef.current = false
            return
          }
          try {
            const bytes = await blob.arrayBuffer()
            const params = new URLSearchParams({
              camera_id: String(cameraID),
              stream_id: `cam-${cameraID}`,
              imgsz: '640',
              conf: '0.25',
            })
            if (Array.isArray(normalizedLocalGeometry?.lanes) && normalizedLocalGeometry.lanes.length > 0) {
              params.set('lanes', JSON.stringify(normalizedLocalGeometry.lanes))
            }
            const url = `${apiBase}/api/v1/pipeline/focus/detect?${params.toString()}`
            const resp = await fetch(url, {
              method: 'POST',
              headers: { 'Content-Type': 'application/octet-stream' },
              body: bytes,
            })
            if (!resp.ok) {
              throw new Error(`detect failed (${resp.status})`)
            }
            const payload = await resp.json()
            latestDetectionRef.current = payload
            detectionCbRef.current?.(payload)
          } catch {
            // Keep rendering stream even if detector sidecar is unavailable.
          } finally {
            detectInflightRef.current = false
          }
        }, 'image/jpeg', 0.82)
      }

      rafRef.current = requestAnimationFrame(tick)
    }

    lastFrameRef.current = 0
    rafRef.current = requestAnimationFrame(tick)
    return () => {
      cancelAnimationFrame(rafRef.current)
      prevGrayRef.current = null
    }
  }, [fps, detectorFPS, streamUrl, cameraID, apiBase, normalizedLocalGeometry, laneEditMode])

  return (
    <div className="focusStreamFrames">
      <video
        ref={videoRef}
        className="focusVideoHidden"
        playsInline
        muted
        autoPlay
        aria-hidden
      />
      <canvas ref={metricsCanvasRef} className="focusMetricsCanvas" aria-hidden />
      {hlsError && <p className="focusHlsError">{hlsError}</p>}
      {!streamReady && !hlsError && <p className="focusPlaceholder">Connecting to stream…</p>}
      {laneEditMode && (
        <div className="laneEditorPanel">
          <div className="laneEditorButtons">
            <button type="button" onClick={commitDraftLane}>
              Add lane from points
            </button>
            <button type="button" onClick={clearDraftLane}>
              Clear draft
            </button>
            <button type="button" onClick={removeLastLane}>
              Remove last lane
            </button>
          </div>
          <p className="laneEditorHint">
            Click the processed canvas to place polygon points, then add lane. Local-only for now.
          </p>
          {laneEditError && <p className="focusHlsError">{laneEditError}</p>}
        </div>
      )}
      <div className="focusGrid focusGridLive">
        <div className="focusCard focusCardLive">
          <h3>Raw (stream)</h3>
          <canvas ref={rawRef} className="focusFrameCanvas" />
        </div>
        <div className="focusCard focusCardLive focusCardLiveProcessed">
          <h3>Processed (YOLO)</h3>
          <canvas
            ref={procRef}
            className="focusFrameCanvas focusFrameCanvasProcessed"
            onClick={handleCanvasClick}
          />
        </div>
      </div>
    </div>
  )
}
