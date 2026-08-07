import { describe, expect, it } from 'vitest'

import { sparklinePoints, sparklinePolyline } from './sparkline'

describe('sparklinePoints', () => {
  it('returns a flat 2-point midline for zero samples (the seed state)', () => {
    const points = sparklinePoints([], 60, 20)
    expect(points).toEqual([
      { x: 0, y: 10 },
      { x: 60, y: 10 },
    ])
  })

  it('returns a flat 2-point midline for exactly one sample', () => {
    const points = sparklinePoints([42], 60, 20)
    expect(points).toEqual([
      { x: 0, y: 10 },
      { x: 60, y: 10 },
    ])
  })

  it('returns a flat midline for a constant series (min === max)', () => {
    const points = sparklinePoints([5, 5, 5], 60, 20, 0)
    for (const p of points) {
      expect(p.y).toBe(10)
    }
  })

  it('maps the minimum sample to the bottom (height - padding) and the maximum to the top (padding)', () => {
    const points = sparklinePoints([0, 100], 60, 20, 2)
    expect(points[0]).toEqual({ x: 0, y: 18 }) // min -> bottom
    expect(points[1]).toEqual({ x: 60, y: 2 }) // max -> top
  })

  it('spaces x coordinates evenly across the full width', () => {
    const points = sparklinePoints([1, 2, 3, 4, 5], 100, 20, 0)
    expect(points.map((p) => p.x)).toEqual([0, 25, 50, 75, 100])
  })

  it('places a mid-range sample proportionally between top and bottom', () => {
    const points = sparklinePoints([0, 50, 100], 60, 20, 0)
    // 50 is exactly halfway between the 0 min and 100 max -> exact
    // vertical midpoint.
    expect(points[1]!.y).toBe(10)
  })
})

describe('sparklinePolyline', () => {
  it('renders points as a space-separated "x,y" attribute value', () => {
    const got = sparklinePolyline([
      { x: 0, y: 10 },
      { x: 30, y: 5 },
      { x: 60, y: 15 },
    ])
    expect(got).toBe('0,10 30,5 60,15')
  })

  it('rounds to 2 decimal places', () => {
    const got = sparklinePolyline([{ x: 1.23456, y: 7.8999 }])
    expect(got).toBe('1.23,7.9')
  })

  it('renders an empty points array as an empty string', () => {
    expect(sparklinePolyline([])).toBe('')
  })
})
