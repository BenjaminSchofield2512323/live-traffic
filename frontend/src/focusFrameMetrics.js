/** Mirrors backend/cmd/api/pipeline.go deriveMetrics + edgeDensity + overlay math. */

export function clamp(v, lo, hi) {
  return Math.min(hi, Math.max(lo, v))
}

export function edgeDensity(gray, w, h) {
  const startY = Math.floor(h / 3)
  let edges = 0
  let pixels = 0
  for (let y = startY + 1; y < h - 1; y++) {
    for (let x = 1; x < w - 1; x++) {
      const i = y * w + x
      const gx = Math.abs(gray[i + 1] - gray[i - 1])
      const gy = Math.abs(gray[i + w] - gray[i - w])
      if (gx + gy > 42) edges++
      pixels++
    }
  }
  return pixels ? edges / pixels : 0
}

export function deriveMetrics(prev, curr, w, h) {
  if (!curr || curr.length === 0) return { motion: 0, occupancy: 0 }
  if (!prev || prev.length !== curr.length) {
    return { motion: 0.05, occupancy: edgeDensity(curr, w, h) }
  }
  let totalDiff = 0
  for (let i = 0; i < curr.length; i++) {
    const d = Math.abs(curr[i] - prev[i])
    totalDiff += d / 255
  }
  const motion = totalDiff / curr.length
  const occupancy = edgeDensity(curr, w, h)
  return { motion, occupancy }
}

/** RGBA ImageData -> grayscale (length width*height) */
export function imageDataToGray(imageData) {
  const { data, width: w, height: h } = imageData
  const out = new Uint8Array(w * h)
  for (let i = 0, j = 0; j < w * h; i += 4, j++) {
    out[j] = Math.round(0.299 * data[i] + 0.587 * data[i + 1] + 0.114 * data[i + 2])
  }
  return out
}

export function drawProcessedOverlay(ctx, dw, dh, motion, occupancy) {
  const overlayH = Math.max(26, Math.floor(dh / 9))
  ctx.fillStyle = 'rgba(0,0,0,0.7)'
  ctx.fillRect(0, 0, dw, overlayH)

  const motionRatio = clamp(motion * 8, 0, 1)
  const occRatio = clamp(occupancy * 2.2, 0, 1)
  const barW = Math.max(50, Math.floor(dw / 3))
  const barH = Math.max(6, Math.floor(overlayH / 4))
  const padding = Math.max(8, Math.floor(dw / 60))
  const top = Math.max(6, Math.floor(overlayH / 5))

  ctx.fillStyle = 'rgba(70,70,70,0.86)'
  ctx.fillRect(padding, top, barW, barH)
  ctx.fillStyle = 'rgba(45,210,80,1)'
  ctx.fillRect(padding, top, barW * motionRatio, barH)

  const top2 = top + barH + Math.max(4, Math.floor(overlayH / 8))
  ctx.fillStyle = 'rgba(70,70,70,0.86)'
  ctx.fillRect(padding, top2, barW, barH)
  ctx.fillStyle = 'rgba(240,120,20,1)'
  ctx.fillRect(padding, top2, barW * occRatio, barH)

  if (occupancy > 0.24 && motion < 0.03) {
    ctx.strokeStyle = 'rgba(230,35,35,1)'
    ctx.lineWidth = 4
    ctx.strokeRect(2, 2, dw - 4, dh - 4)
  }
}
