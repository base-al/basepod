// Pure SVG-polyline math for the apps-list CPU sparkline (see
// components/CpuSparkline.vue). Kept out of the .vue file, framework-free,
// so it's covered by an ordinary vitest unit test the way every other
// lib/*.ts helper in this repo is — there is no component-rendering test
// harness here (no *.spec.ts under components/ or pages/), so the actual
// math lives here instead of only being exercisable through a rendered
// component.

export interface SparklinePoint {
  x: number
  y: number
}

/**
 * Maps a series of CPU-percent samples (oldest first) onto SVG viewport
 * coordinates (0,0)-(width,height), Y inverted (a higher reading gets a
 * SMALLER y, matching SVG's downward-growing y axis) so a rising trend
 * reads as a rising line the way a human expects. `padding` reserves that
 * many px of headroom top and bottom so a sample sitting at the series'
 * exact min/max doesn't clip against the stroke width or the endpoint
 * dot's radius.
 *
 * Fewer than 2 samples (nothing has arrived yet, or exactly one sample
 * has) returns a flat 2-point line at the vertical midpoint — the "seed"
 * state Apps.vue relies on so a row doesn't jump the instant its first
 * real sample lands (there's nothing to draw a trend from yet with 0 or 1
 * points).
 *
 * A series where every sample is identical (min === max — most visibly,
 * an idle app reading a flat 0% the whole window) would divide by zero
 * computing where between min/max each point falls; that case also
 * renders as a flat line at the midpoint instead.
 */
export function sparklinePoints(samples: number[], width: number, height: number, padding = 2): SparklinePoint[] {
  if (samples.length < 2) {
    const y = height / 2
    return [
      { x: 0, y },
      { x: width, y },
    ]
  }

  const min = Math.min(...samples)
  const max = Math.max(...samples)
  const range = max - min

  const usableHeight = height - padding * 2
  const step = width / (samples.length - 1)

  return samples.map((v, i) => {
    const t = range === 0 ? 0.5 : (v - min) / range
    return { x: i * step, y: padding + (1 - t) * usableHeight }
  })
}

/** Renders sparklinePoints' output as an SVG `<polyline>` `points`
 * attribute value ("x,y x,y ..."), rounded to 2 decimal places — plenty
 * of precision for an ~60px-wide sparkline, and short enough to keep the
 * attribute readable in devtools. */
export function sparklinePolyline(points: SparklinePoint[]): string {
  return points.map((p) => `${round(p.x)},${round(p.y)}`).join(' ')
}

function round(n: number): number {
  return Math.round(n * 100) / 100
}
