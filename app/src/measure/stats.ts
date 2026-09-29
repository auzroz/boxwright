// The few statistics both measurements are built from: percentiles, the peaks
// of a height histogram, a least-squares plane, and an oriented footprint.

/** The q-quantile (0..1) of an ASCENDING sorted array, linearly interpolated. */
export function quantileSorted(sorted: ArrayLike<number>, q: number): number {
  const n = sorted.length;
  if (n === 0) return NaN;
  const pos = Math.min(n - 1, Math.max(0, q * (n - 1)));
  const lo = Math.floor(pos);
  const hi = Math.min(n - 1, lo + 1);
  const a = sorted[lo] as number;
  const b = sorted[hi] as number;
  return a + (b - a) * (pos - lo);
}

/** The q-quantile of unsorted values. Copies; the input is left alone. */
export function quantile(values: ArrayLike<number>, q: number): number {
  const s = Float64Array.from(values as ArrayLike<number>);
  s.sort();
  return quantileSorted(s, q);
}

export interface Peak {
  /** Mean height of the points in the peak's bin and its two neighbours. */
  height: number;
  /** Points in those three bins. */
  support: number;
}

/**
 * The peaks of a 1 cm histogram of heights, lowest first.
 *
 * The histogram is smoothed [1 2 1] first: a flat surface under per-pixel
 * noise of about a centimetre straddles two or three bins, and without it one
 * surface can show up as two neighbouring "peaks". A peak is a local maximum
 * of the smoothed counts holding at least `minShare` of the largest one, and
 * `minPoints` raw points across its three bins -- so the thin noise tails
 * either side of a real surface never qualify on their own.
 */
export function histogramPeaks(
  heights: ArrayLike<number>,
  opts: { bin?: number; minShare: number; minPoints: number },
): Peak[] {
  const bin = opts.bin ?? 0.01;
  const n = heights.length;
  if (n === 0) return [];
  let lo = Infinity;
  let hi = -Infinity;
  for (let i = 0; i < n; i++) {
    const v = heights[i] as number;
    if (v < lo) lo = v;
    if (v > hi) hi = v;
  }
  const bins = Math.max(1, Math.floor((hi - lo) / bin) + 1);
  const counts = new Float64Array(bins);
  const sums = new Float64Array(bins);
  for (let i = 0; i < n; i++) {
    const v = heights[i] as number;
    const b = Math.min(bins - 1, Math.floor((v - lo) / bin));
    counts[b] = (counts[b] as number) + 1;
    sums[b] = (sums[b] as number) + v;
  }
  const at = (a: Float64Array, i: number) => (i >= 0 && i < bins ? (a[i] as number) : 0);
  const smooth = new Float64Array(bins);
  let max = 0;
  for (let b = 0; b < bins; b++) {
    const s = (at(counts, b - 1) + 2 * at(counts, b) + at(counts, b + 1)) / 4;
    smooth[b] = s;
    if (s > max) max = s;
  }
  const peaks: Peak[] = [];
  for (let b = 0; b < bins; b++) {
    const s = smooth[b] as number;
    // >= on the left and > on the right, so a flat-topped peak is reported once.
    if (s < at(smooth, b - 1) || s <= at(smooth, b + 1)) continue;
    if (s < opts.minShare * max) continue;
    const support = at(counts, b - 1) + at(counts, b) + at(counts, b + 1);
    if (support < opts.minPoints) continue;
    const sum = at(sums, b - 1) + at(sums, b) + at(sums, b + 1);
    peaks.push({ height: sum / support, support });
  }
  return peaks;
}

/**
 * Least-squares fit of h = a*s + b*t + c over paired samples.
 *
 * A plane written as a height over two horizontal coordinates rather than a
 * general 3-D fit: the support surface is close to horizontal by assumption,
 * and this form makes "how far from horizontal is it" simply atan(|(a, b)|).
 * Null when the points are degenerate (all on a line).
 */
export function fitHeightPlane(
  s: ArrayLike<number>,
  t: ArrayLike<number>,
  h: ArrayLike<number>,
): { a: number; b: number; c: number } | null {
  const n = h.length;
  if (n < 3) return null;
  // Centre first, for a well-conditioned 2x2 solve.
  let ms = 0;
  let mt = 0;
  let mh = 0;
  for (let i = 0; i < n; i++) {
    ms += s[i] as number;
    mt += t[i] as number;
    mh += h[i] as number;
  }
  ms /= n;
  mt /= n;
  mh /= n;
  let sss = 0;
  let stt = 0;
  let sst = 0;
  let ssh = 0;
  let sth = 0;
  for (let i = 0; i < n; i++) {
    const ds = (s[i] as number) - ms;
    const dt = (t[i] as number) - mt;
    const dh = (h[i] as number) - mh;
    sss += ds * ds;
    stt += dt * dt;
    sst += ds * dt;
    ssh += ds * dh;
    sth += dt * dh;
  }
  const det = sss * stt - sst * sst;
  if (!(Math.abs(det) > 1e-12)) return null;
  const a = (ssh * stt - sth * sst) / det;
  const b = (sth * sss - ssh * sst) / det;
  return { a, b, c: mh - a * ms - b * mt };
}

/** A rectangle on a plane: centre, unit axis `u` (and its perpendicular), and extents. */
export interface OrientedBox {
  cs: number;
  ct: number;
  /** Direction of the LONG side, as a unit (s, t). */
  us: number;
  ut: number;
  long: number;
  short: number;
}

/** Principal axis angle (radians) of 2-D points. */
export function pcaAngle(s: ArrayLike<number>, t: ArrayLike<number>): number {
  const n = s.length;
  let ms = 0;
  let mt = 0;
  for (let i = 0; i < n; i++) {
    ms += s[i] as number;
    mt += t[i] as number;
  }
  ms /= n || 1;
  mt /= n || 1;
  let css = 0;
  let ctt = 0;
  let cst = 0;
  for (let i = 0; i < n; i++) {
    const ds = (s[i] as number) - ms;
    const dt = (t[i] as number) - mt;
    css += ds * ds;
    ctt += dt * dt;
    cst += ds * dt;
  }
  return 0.5 * Math.atan2(2 * cst, css - ctt);
}

/**
 * The footprint of 2-D points: the rectangle their `lo`..`hi` quantiles span,
 * oriented to make it smallest.
 *
 * PCA alone gives the orientation of a long rectangle, but not of a square
 * one: a square's spread is the same in every direction, so its principal
 * axis is noise, and extents taken along a diagonal read up to 41% large. So
 * PCA only SEEDS a search of +/-45 degrees in 1-degree steps -- which covers
 * every orientation a rectangle can have -- for the least-area trimmed
 * rectangle. On a rectangle that is exactly the true orientation.
 */
export function orientedFootprint(
  s: ArrayLike<number>,
  t: ArrayLike<number>,
  lo: number,
  hi: number,
): OrientedBox {
  const n = s.length;
  const seed = pcaAngle(s, t);
  const ps = new Float64Array(n);
  const pt = new Float64Array(n);
  let best: { area: number; angle: number; a0: number; a1: number; b0: number; b1: number } | null = null;
  for (let step = -45; step < 45; step++) {
    const angle = seed + (step * Math.PI) / 180;
    const c = Math.cos(angle);
    const sn = Math.sin(angle);
    for (let i = 0; i < n; i++) {
      const si = s[i] as number;
      const ti = t[i] as number;
      ps[i] = si * c + ti * sn;
      pt[i] = -si * sn + ti * c;
    }
    ps.sort();
    pt.sort();
    const a0 = quantileSorted(ps, lo);
    const a1 = quantileSorted(ps, hi);
    const b0 = quantileSorted(pt, lo);
    const b1 = quantileSorted(pt, hi);
    const area = (a1 - a0) * (b1 - b0);
    if (best === null || area < best.area) best = { area, angle, a0, a1, b0, b1 };
  }
  const b = best as NonNullable<typeof best>;
  const c = Math.cos(b.angle);
  const sn = Math.sin(b.angle);
  const ma = (b.a0 + b.a1) / 2;
  const mb = (b.b0 + b.b1) / 2;
  // Back from the rotated frame to (s, t).
  const cs = ma * c - mb * sn;
  const ct = ma * sn + mb * c;
  const ea = b.a1 - b.a0;
  const eb = b.b1 - b.b0;
  return ea >= eb
    ? { cs, ct, us: c, ut: sn, long: ea, short: eb }
    : { cs, ct, us: -sn, ut: c, long: eb, short: ea };
}

/** Clamps x into [lo, hi]. */
export function clamp(x: number, lo: number, hi: number): number {
  return Math.min(hi, Math.max(lo, x));
}
