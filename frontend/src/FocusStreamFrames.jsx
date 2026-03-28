import { useEffect, useMemo, useRef, useState } from 'react'
import Hls, { XhrLoader } from 'hls.js'
import { deriveMetrics, drawProcessedOverlay, imageDataToGray } from './focusFrameMetrics.js'

const MW = 64
const MH = 36
const ASSOC_COMPARISON_MODES = [
  { key: 'legacy', hungarianEnabled: false },
  { key: 'hungarian', hungarianEnabled: true },
]

/**
 * Map pointer coordinates to canvas bitmap pixels when the element is CSS-sized with
 * `object-fit: contain` (letterboxing). Clicks on the padded bands must not map to the image.
 */
function clientPointToCanvasPixels(canvas, clientX, clientY) {
  const rect = canvas.getBoundingClientRect()
  const iw = canvas.width
  const ih = canvas.height
  if (rect.width <= 0 || rect.height <= 0 || iw <= 0 || ih <= 0) return null
  const sx = clientX - rect.left
  const sy = clientY - rect.top
  const scale = Math.min(rect.width / iw, rect.height / ih)
  const dispW = iw * scale
  const dispH = ih * scale
  const offX = (rect.width - dispW) / 2
  const offY = (rect.height - dispH) / 2
  const lx = sx - offX
  const ly = sy - offY
  if (lx < 0 || ly < 0 || lx > dispW || ly > dispH) return null
  const px = (lx / dispW) * iw
  const py = (ly / dispH) * ih
  return {
    x: Math.max(0, Math.min(iw - 1, Math.round(px))),
    y: Math.max(0, Math.min(ih - 1, Math.round(py))),
  }
}

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

function drawTrackOverlay(ctx, payload, dw, dh, hasLocalLanes) {
  const { image: detImage, detections: detList } = normalizeDetectorPayload(payload)
  const trackList = Array.isArray(payload?.tracks) ? payload.tracks : []
  const overlayItems = trackList.length > 0 ? trackList : detList
  if (!Array.isArray(overlayItems) || overlayItems.length === 0) return

  const detW = Number(detImage?.width || 0) || dw
  const detH = Number(detImage?.height || 0) || dh
  const sx = detW > 0 ? dw / detW : 1
  const sy = detH > 0 ? dh / detH : 1
  ctx.lineWidth = 2
  ctx.strokeStyle = 'rgba(45,210,80,0.95)'
  ctx.font = '12px sans-serif'
  ctx.fillStyle = 'rgba(45,210,80,0.95)'
  overlayItems.forEach((d) => {
    if (!Array.isArray(d.bbox) || d.bbox.length < 4) return
    const laneID = d.lane_id
    const inROI = Boolean(d.in_roi ?? (laneID != null && laneID !== ''))
    if (hasLocalLanes && !inROI) return
    const [x1, y1, x2, y2] = d.bbox
    const bx = x1 * sx
    const by = y1 * sy
    const bw = Math.max(1, (x2 - x1) * sx)
    const bh = Math.max(1, (y2 - y1) * sy)
    ctx.strokeRect(bx, by, bw, bh)
    const tag = trackNumberToLabel(d.track_id)
    ctx.fillText(tag, bx + 2, Math.max(12, by - 3))
  })
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

/** Same-origin proxy; browsers cannot set Referer on XHR (unsafe header). */
function earthCamHlsProxyUrl(apiBase, targetUrl) {
  if (!targetUrl) return ''
  const base = apiBase.endsWith('/') ? apiBase.slice(0, -1) : apiBase
  return `${base}/api/v1/stream/hls-proxy?u=${encodeURIComponent(targetUrl)}`
}

function createEarthCamProxyLoader(apiBase) {
  return class EarthCamProxyLoader extends XhrLoader {
    load(context, config, callbacks) {
      const u = context.url
      if (typeof u === 'string' && u.includes('/api/v1/stream/hls-proxy?')) {
        return super.load(context, config, callbacks)
      }
      if (typeof u === 'string' && u.includes('earthcam.com')) {
        const proxied = earthCamHlsProxyUrl(apiBase, u)
        return super.load({ ...context, url: proxied }, config, callbacks)
      }
      return super.load(context, config, callbacks)
    }
  }
}

/**
 * Hidden HLS video decodes the stream; two canvases show raw vs processed (overlay)
 * at the requested FPS — no visible video player.
 */
export function FocusStreamFrames({
  streamUrl,
  fps,
  detectorFPS = 2,
  detectConf = 0.25,
  detectIou = 0.45,
  detectImgsz = 640,
  trackAssocIou = 0.25,
  trackAssocCenterPx = 96,
  detectClasses = '',
  localLaneGeometry = null,
  laneEditMode = false,
  onLaneGeometryChange,
  cameraID,
  apiBase,
  onFrameMetrics,
  onDetectionMetrics,
  /** Route EarthCam HLS through the Go API so Referer can be set server-side. */
  earthCamHlsProxy = false,
}) {
  const videoRef = useRef(null)
  const hlsRef = useRef(null)
  const rawRef = useRef(null)
  const procLegacyRef = useRef(null)
  const procHungarianRef = useRef(null)
  const metricsCanvasRef = useRef(null)
  const prevGrayRef = useRef(null)
  const rafRef = useRef(0)
  const lastFrameRef = useRef(0)
  const metricsCbRef = useRef(onFrameMetrics)
  const detectionCbRef = useRef(onDetectionMetrics)
  const detectInflightRef = useRef(false)
  const lastDetectAtRef = useRef(0)
  const latestDetectionRef = useRef({
    legacy: null,
    hungarian: null,
  })
  const analysisFrameRef = useRef({
    legacy: null,
    hungarian: null,
  })
  const detectTuningRef = useRef({
    conf: detectConf,
    iou: detectIou,
    imgsz: detectImgsz,
    trackAssocIou,
    trackAssocCenterPx,
    detectClasses,
  })
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
    detectTuningRef.current = {
      conf: detectConf,
      iou: detectIou,
      imgsz: detectImgsz,
      trackAssocIou,
      trackAssocCenterPx,
      detectClasses,
    }
  }, [detectConf, detectIou, detectImgsz, trackAssocIou, trackAssocCenterPx, detectClasses])

  useEffect(() => {
    latestDetectionRef.current = { legacy: null, hungarian: null }
    analysisFrameRef.current = { legacy: null, hungarian: null }
    detectInflightRef.current = false
    lastDetectAtRef.current = 0
    detectionCbRef.current?.(null)
    draftPolyRef.current = []
  }, [streamUrl, cameraID, localLaneGeometry])

  const handleCanvasClick = (evt) => {
    if (!laneEditMode) return
    const proc = procLegacyRef.current
    if (!proc) return
    const pt = clientPointToCanvasPixels(proc, evt.clientX, evt.clientY)
    if (!pt) return
    draftPolyRef.current = [...draftPolyRef.current, [pt.x, pt.y]]
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

    const safariUrl =
      earthCamHlsProxy && url.includes('earthcam.com') ? earthCamHlsProxyUrl(apiBase, url) : url

    if (Hls.isSupported()) {
      const LoaderClass = earthCamHlsProxy ? createEarthCamProxyLoader(apiBase) : undefined
      const hls = new Hls({
        enableWorker: true,
        lowLatencyMode: true,
        ...(LoaderClass ? { loader: LoaderClass } : {}),
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
      video.src = safariUrl
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
  }, [streamUrl, earthCamHlsProxy, apiBase])

  useEffect(() => {
    const video = videoRef.current
    const raw = rawRef.current
    const procLegacy = procLegacyRef.current
    const procHungarian = procHungarianRef.current
    const metricsCanvas = metricsCanvasRef.current
    if (!video || !raw || !procLegacy || !procHungarian || !metricsCanvas) return undefined

    const ctxM = metricsCanvas.getContext('2d', { willReadFrequently: true })
    const intervalMs = 1000 / Math.max(1, Math.min(30, fps))
    const boundedDetectorFPS = Math.max(1, Math.min(30, Number(detectorFPS) || 2))
    const detectorIntervalMs = 1000 / boundedDetectorFPS

    const drawModeCanvas = (canvas, rawPayload, frozenCanvas, dw, dh) => {
      const procCtx = canvas.getContext('2d')
      if (!procCtx) return

      if (frozenCanvas && frozenCanvas.width > 0 && frozenCanvas.height > 0) {
        procCtx.drawImage(frozenCanvas, 0, 0, dw, dh)
      } else {
        procCtx.drawImage(video, 0, 0, dw, dh)
      }

      drawProcessedOverlay(procCtx, dw, dh, 0, 0)

      const { image: detImage, detections: detList } = normalizeDetectorPayload(rawPayload)
      const trackList = Array.isArray(rawPayload?.tracks) ? rawPayload.tracks : []
      const hasLocalLanes = Array.isArray(normalizedLocalGeometry?.lanes) && normalizedLocalGeometry.lanes.length > 0
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

      const overlayItems = trackList.length > 0 ? trackList : detList
      if (Array.isArray(overlayItems) && overlayItems.length > 0) {
        const detW = Number(detImage?.width || 0) || dw
        const detH = Number(detImage?.height || 0) || dh
        const sx = detW > 0 ? dw / detW : 1
        const sy = detH > 0 ? dh / detH : 1

        procCtx.lineWidth = 2
        procCtx.strokeStyle = 'rgba(45,210,80,0.95)'
        procCtx.font = '12px sans-serif'
        procCtx.fillStyle = 'rgba(45,210,80,0.95)'
        overlayItems.forEach((d) => {
          if (!Array.isArray(d.bbox) || d.bbox.length < 4) return
          const laneID = d.lane_id
          const inROI = Boolean(d.in_roi ?? (laneID != null && laneID !== ''))
          if (hasLocalLanes && !inROI) return
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
    }

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
      procLegacy.width = dw
      procLegacy.height = dh
      procHungarian.width = dw
      procHungarian.height = dh
      metricsCanvas.width = MW
      metricsCanvas.height = MH

      const rawCtx = raw.getContext('2d')

      rawCtx.drawImage(video, 0, 0, dw, dh)
      ctxM.drawImage(video, 0, 0, MW, MH)
      const imgData = ctxM.getImageData(0, 0, MW, MH)
      const gray = imageDataToGray(imgData)
      const prev = prevGrayRef.current
      const { motion, occupancy } = deriveMetrics(prev, gray, MW, MH)
      prevGrayRef.current = gray

      const legacyPayload = latestDetectionRef.current.legacy
      const hungarianPayload = latestDetectionRef.current.hungarian || legacyPayload
      drawModeCanvas(procLegacy, legacyPayload, analysisFrameRef.current.legacy, dw, dh)
      drawModeCanvas(procHungarian, hungarianPayload, analysisFrameRef.current.hungarian, dw, dh)
      metricsCbRef.current?.({ motion, occupancy })

      const shouldDetect =
        cameraID &&
        !detectInflightRef.current &&
        now - lastDetectAtRef.current >= detectorIntervalMs
      if (shouldDetect) {
        detectInflightRef.current = true
        lastDetectAtRef.current = now
        const analysisCanvas = document.createElement('canvas')
        analysisCanvas.width = dw
        analysisCanvas.height = dh
        const analysisCtx = analysisCanvas.getContext('2d')
        if (!analysisCtx) {
          detectInflightRef.current = false
          rafRef.current = requestAnimationFrame(tick)
          return
        }
        analysisCtx.drawImage(video, 0, 0, dw, dh)
        analysisCanvas.toBlob(async (blob) => {
          if (!blob) {
            detectInflightRef.current = false
            return
          }
          try {
            const bytes = await blob.arrayBuffer()
            const tun = detectTuningRef.current
            const imgsz = Math.round(Math.max(160, Math.min(1280, Number(tun.imgsz) || 640)) / 32) * 32
            const baseParams = {
              camera_id: String(cameraID),
              stream_id: `cam-${cameraID}`,
              imgsz: String(imgsz),
              conf: String(tun.conf),
              iou: String(tun.iou),
              track_assoc_iou_threshold: String(tun.trackAssocIou),
              track_assoc_center_max_px: String(tun.trackAssocCenterPx),
              enhanced_preview: '1',
            }
            if (String(tun.detectClasses || '').trim()) {
              baseParams.classes = String(tun.detectClasses).trim()
            }
            const lanesJSON = Array.isArray(normalizedLocalGeometry?.lanes) && normalizedLocalGeometry.lanes.length > 0
              ? JSON.stringify(normalizedLocalGeometry.lanes)
              : ''
            const runDetect = async (useHungarian) => {
              const params = new URLSearchParams({
                ...baseParams,
                track_assoc_hungarian_enabled: useHungarian ? '1' : '0',
              })
              if (lanesJSON) params.set('lanes', lanesJSON)
              const url = `${apiBase}/api/v1/pipeline/focus/detect?${params.toString()}`
              const resp = await fetch(url, {
                method: 'POST',
                headers: { 'Content-Type': 'application/octet-stream' },
                body: bytes,
              })
              if (!resp.ok) {
                throw new Error(`detect failed (${resp.status})`)
              }
              return resp.json()
            }
            const [legacyPayload, hungarianPayload] = await Promise.all([
              runDetect(false),
              runDetect(true),
            ])
            analysisFrameRef.current.legacy = analysisCanvas
            analysisFrameRef.current.hungarian = analysisCanvas
            latestDetectionRef.current = {
              legacy: legacyPayload,
              hungarian: hungarianPayload,
            }
            detectionCbRef.current?.(hungarianPayload || legacyPayload)
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
      </div>
      <div className="focusCompareRows">
        <div className="focusCard focusCardLive focusCardLiveProcessed focusCompareCard">
          <h3>Processed (Legacy/Greedy)</h3>
          <canvas
            ref={procLegacyRef}
            className="focusFrameCanvas focusFrameCanvasProcessed"
            onClick={handleCanvasClick}
          />
        </div>
        <div className="focusCard focusCardLive focusCardLiveProcessed focusCompareCard">
          <h3>Processed (Hungarian)</h3>
          <canvas
            ref={procHungarianRef}
            className="focusFrameCanvas focusFrameCanvasProcessed focusFrameCanvasProcessedHungarian"
          />
        </div>
      </div>
    </div>
  )
}
