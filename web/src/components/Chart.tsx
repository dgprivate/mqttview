import type { SeriesPoint } from '../api/types'

/**
 * Chart draws a numeric series as inline SVG.
 *
 * Inline rather than a charting library: the whole frontend is one bundle
 * served from the binary, and a library for one line graph would be a
 * meaningful fraction of it. It also keeps the strict Content-Security-Policy
 * intact — nothing is fetched, nothing is evaluated.
 *
 * The viewBox does the scaling, so the drawing is resolution-independent and
 * the same component works on a phone and on a wall display.
 */
export function Chart({
  points,
  height = 160,
  label,
}: {
  points: SeriesPoint[]
  height?: number
  label?: string
}) {
  if (points.length === 0) {
    return <p className="subtitle">No numeric values in this window.</p>
  }
  // One point is a reading, not a series. Drawing a line through it would
  // suggest a trend that nothing supports.
  if (points.length === 1) {
    return (
      <p className="subtitle">
        A single reading of <strong>{formatValue(points[0].v)}</strong> at{' '}
        {new Date(points[0].t).toLocaleTimeString()} — not enough for a chart yet.
      </p>
    )
  }

  const width = 600
  const padding = { top: 8, right: 8, bottom: 18, left: 44 }
  const plotWidth = width - padding.left - padding.right
  const plotHeight = height - padding.top - padding.bottom

  const values = points.map((p) => p.v)
  let min = Math.min(...values)
  let max = Math.max(...values)
  // A flat series has no range to scale to; without this every point lands on
  // the same pixel row and the line vanishes.
  if (min === max) {
    min -= 1
    max += 1
  }

  const times = points.map((p) => new Date(p.t).getTime())
  const firstTime = times[0]
  const lastTime = times[times.length - 1]
  const timeSpan = lastTime - firstTime || 1

  const x = (t: number) => padding.left + ((t - firstTime) / timeSpan) * plotWidth
  const y = (v: number) => padding.top + (1 - (v - min) / (max - min)) * plotHeight

  const path = points.map((p, i) => `${i === 0 ? 'M' : 'L'}${x(times[i]).toFixed(2)},${y(p.v).toFixed(2)}`).join(' ')

  const last = points[points.length - 1]

  return (
    <figure className="chart">
      <svg
        viewBox={`0 0 ${width} ${height}`}
        preserveAspectRatio="none"
        role="img"
        aria-label={
          label
            ? `${label}: ${points.length} readings between ${formatValue(min)} and ${formatValue(max)}`
            : `${points.length} readings between ${formatValue(min)} and ${formatValue(max)}`
        }
      >
        {/* Horizontal guides at the extremes and the middle. */}
        {[0, 0.5, 1].map((fraction) => {
          const value = min + (max - min) * (1 - fraction)
          const gy = padding.top + fraction * plotHeight
          return (
            <g key={fraction}>
              <line
                x1={padding.left}
                x2={width - padding.right}
                y1={gy}
                y2={gy}
                className="chart-grid"
              />
              <text x={padding.left - 6} y={gy + 3} textAnchor="end" className="chart-label">
                {formatValue(value)}
              </text>
            </g>
          )
        })}
        <path d={path} className="chart-line" fill="none" />
        <circle cx={x(lastTime)} cy={y(last.v)} r="3" className="chart-point" />
      </svg>
      <figcaption className="subtitle">
        {points.length.toLocaleString()} readings · latest <strong>{formatValue(last.v)}</strong> at{' '}
        {new Date(last.t).toLocaleTimeString()}
      </figcaption>
    </figure>
  )
}

/** formatValue keeps a number readable without pretending to a precision the
 *  reading does not have. */
function formatValue(v: number): string {
  if (Number.isInteger(v)) return v.toLocaleString()
  if (Math.abs(v) >= 1000) return v.toFixed(0)
  if (Math.abs(v) >= 1) return v.toFixed(2)
  return v.toPrecision(3)
}
