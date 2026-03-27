import { useEffect, useRef, useState } from 'react'
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
  const [streamReady, setStreamReady] = useState(false)
  const [hlsError, setHlsError] = useState(null)

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
  }, [streamUrl, cameraID])

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
          const tag = `${d.class_name || 'obj'} ${(Number(d.confidence || 0) * 100).toFixed(0)}%`
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
            const url = `${apiBase}/api/v1/pipeline/focus/detect?camera_id=${cameraID}&stream_id=cam-${cameraID}&imgsz=640&conf=0.25`
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
  }, [fps, detectorFPS, streamUrl, cameraID, apiBase])

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
      <div className="focusGrid focusGridLive">
        <div className="focusCard focusCardLive">
          <h3>Raw (stream)</h3>
          <canvas ref={rawRef} className="focusFrameCanvas" />
        </div>
        <div className="focusCard focusCardLive focusCardLiveProcessed">
          <h3>Processed (YOLO)</h3>
          <canvas ref={procRef} className="focusFrameCanvas focusFrameCanvasProcessed" />
        </div>
      </div>
    </div>
  )
}
