// An item's size from one LiDAR frame.
//
// The vision model's size is a guess from a picture; this is a measurement,
// but only of what the depth sensor could see. So it always comes with a
// confidence, and the caller keeps the vision estimate when that is low (the
// plan's cut is 0.5). Returning null is ordinary: it means "not enough depth to
// say", never "the item is zero-sized".
//
// Everything is relative to the surface the item stands on, which is found
// first. Gravity is known -- ARKit's world is gravity-aligned -- so finding
// that surface is a one-dimensional problem: a histogram of heights.

import type { Region } from "../types";
import {
  type DepthFrame,
  type PointGrid,
  type Vec3,
  cameraToWorld,
  depthIntrinsics,
  dot,
  isSupportedOrientation,
  normalize,
  padRegion,
  planeBasis,
  pointGrid,
  portraitToSensor,
  sensorRectPixels,
  upVector,
  worldDirToCamera,
  worldToCamera,
} from "./frame";
import { clamp, fitHeightPlane, histogramPeaks, orientedFootprint, quantile } from "./stats";

/** Centimetres, longest first, each to the nearest 0.5 cm. */
export interface MeasuredSize {
  l: number;
  w: number;
  h: number;
  /** 0..1. Below about 0.5, prefer the vision estimate. */
  confidence: number;
}

/** How much of the surroundings to take in around a region, per side. */
const REGION_PAD = 0.25;
/** Anything lower than this above the support is the support (or a sticker on it). */
const OBJECT_MIN_HEIGHT = 0.015;
/** Half-width of the band of heights that counts as "on the support plane". */
const PLANE_BAND = 0.015;
/** A neighbouring pixel further away than this is a different surface. */
const DEPTH_JUMP = 0.03;
/** A support steeper than this is not something an item is standing on. */
const MAX_PLANE_TILT = (15 * Math.PI) / 180;
/** Fewer object points than this and there is nothing to measure. */
const MIN_POINTS = 60;
/** From here up the point count no longer limits confidence. */
const FULL_POINTS = 300;
/** An object narrower than this many depth pixels is only partly resolved. */
const FULL_SPAN_PX = 12;
/**
 * The footprint cap for the orientation search, which sorts every point 180
 * times. A 256x192 map rarely gives an object more than this, and a stride
 * over the rest changes nothing a 2-98% extent can see.
 */
const MAX_FOOTPRINT_POINTS = 4000;

/** How the support surface was found, which is also how much to trust it. */
type PlaneSource = "fit" | "arkit" | "percentile";

interface SupportPlane {
  /** Unit normal in camera space, pointing up. */
  n: Vec3;
  /** Height of point p above the plane is n . p - c. */
  c: number;
  source: PlaneSource;
}

/**
 * Measures the item in `region` of the upright photo -- or, with no region,
 * the object nearest the middle of the frame.
 *
 * Steps, each explained where it happens:
 *  1. the region, padded 25% so it takes in some of the surface around it;
 *  2. that surface: the lowest dominant height in the padding, refined by a
 *     plane fit (then an ARKit plane, then a low percentile, as fallbacks);
 *  3. the object: points 1.5 cm or more above it, as the connected piece the
 *     region is most about, eroded one pixel to lose its ragged edge;
 *  4. height, the 98th percentile above the plane; footprint, the 2-98%
 *     extent along the orientation that makes it smallest.
 */
export function measureItem(frame: DepthFrame, region?: Region): MeasuredSize | null {
  if (!isSupportedOrientation(frame.orientation) || frame.depthW < 3 || frame.depthH < 3) return null;

  // 1. Where to look. Everything is done in the sensor's own orientation;
  // the region is the only thing that arrives in the photo's.
  const whole: Region = { x: 0, y: 0, w: 1, h: 1 };
  const inner = region ? portraitToSensor(region, frame.orientation) : whole;
  const outer = region ? padRegion(inner, REGION_PAD) : whole;
  const innerPx = sensorRectPixels(frame, inner);
  const outerPx = sensorRectPixels(frame, outer);
  if (innerPx.x1 - innerPx.x0 < 2 || innerPx.y1 - innerPx.y0 < 2) return null;

  // Median-smoothed: see pointGrid. Items are many pixels across, so the
  // median costs nothing here and removes the flying pixels along their edges.
  const g = pointGrid(frame, outerPx, true);
  const inInner = (i: number, j: number) => {
    const x = g.rect.x0 + i;
    const y = g.rect.y0 + j;
    return x >= innerPx.x0 && x < innerPx.x1 && y >= innerPx.y0 && y < innerPx.y1;
  };

  // 2. The support surface.
  const plane = supportPlane(frame, g, region ? (i, j) => !inInner(i, j) : () => true, inInner);
  if (!plane) return null;

  // 3. The object: everything standing clear of the surface...
  const n = g.width * g.height;
  const above = new Float32Array(n);
  const mask = new Uint8Array(n);
  for (let k = 0; k < n; k++) {
    if (!g.valid[k]) continue;
    const a = plane.n[0] * (g.x[k] as number) + plane.n[1] * (g.y[k] as number) + plane.n[2] * (g.z[k] as number) - plane.c;
    above[k] = a;
    if (a > OBJECT_MIN_HEIGHT) mask[k] = 1;
  }

  // ...split into connected pieces wherever the depth jumps, so a box in front
  // of another box is two things even where they overlap in the picture.
  const { labels, sizes } = components(g, mask);
  const chosen = region ? mostInRegion(g, labels, sizes, inInner) : nearestCentre(frame, g, labels, sizes);
  if (chosen < 0) return null;

  // Touching the edge of what we looked at means part of it may be outside:
  // the measurement is then a lower bound, and trusted less.
  let truncated = false;
  for (let j = 0; j < g.height && !truncated; j++) {
    for (let i = 0; i < g.width; i++) {
      if (labels[j * g.width + i] !== chosen) continue;
      if (i === 0 || j === 0 || i === g.width - 1 || j === g.height - 1) {
        truncated = true;
        break;
      }
    }
  }

  // Erode one pixel: the outline is where flying pixels and half-floor,
  // half-object readings live, and the percentiles below should not see it.
  const idx: number[] = [];
  let minI = Infinity;
  let maxI = -Infinity;
  let minJ = Infinity;
  let maxJ = -Infinity;
  for (let j = 1; j < g.height - 1; j++) {
    for (let i = 1; i < g.width - 1; i++) {
      const k = j * g.width + i;
      if (labels[k] !== chosen) continue;
      if (labels[k - 1] !== chosen || labels[k + 1] !== chosen) continue;
      if (labels[k - g.width] !== chosen || labels[k + g.width] !== chosen) continue;
      idx.push(k);
      if (i < minI) minI = i;
      if (i > maxI) maxI = i;
      if (j < minJ) minJ = j;
      if (j > maxJ) maxJ = j;
    }
  }
  const count = idx.length;
  if (count < MIN_POINTS) return null;

  // 4. Height, and the footprint on the plane.
  const heights = new Float64Array(count);
  for (let q = 0; q < count; q++) heights[q] = above[idx[q] as number] as number;
  const height = quantile(heights, 0.98);

  const [e1, e2] = planeBasis(plane.n);
  const stride = Math.max(1, Math.ceil(count / MAX_FOOTPRINT_POINTS));
  const s: number[] = [];
  const t: number[] = [];
  for (let q = 0; q < count; q += stride) {
    const k = idx[q] as number;
    const p: Vec3 = [g.x[k] as number, g.y[k] as number, g.z[k] as number];
    s.push(dot(e1, p));
    t.push(dot(e2, p));
  }
  const foot = orientedFootprint(s, t, 0.02, 0.98);
  // Two known shrinkages, put back. The 2-98% range of an evenly sampled
  // surface spans 96% of it, and the erosion took one pixel off each side --
  // one pixel being distance / focal length at this range. Without these, a
  // 30x20 cm box read 28x19 on every synthetic frame.
  const distance = quantile(depthsOf(g, idx), 0.5);
  const pixel = distance / depthIntrinsics(frame).fx;
  const long = foot.long / 0.96 + 2 * pixel;
  const short = foot.short / 0.96 + 2 * pixel;

  // Confidence: a product, so any one weak reason is enough to fall back.
  let high = 0;
  let sx = 0;
  let sy = 0;
  let sz = 0;
  for (let q = 0; q < count; q++) {
    const k = idx[q] as number;
    if (g.conf[k] === 2) high++;
    sx += g.x[k] as number;
    sy += g.y[k] as number;
    sz += g.z[k] as number;
  }
  const centre: Vec3 = [sx, sy, sz];
  const span = Math.min(maxI - minI + 1, maxJ - minJ + 1);

  const countF = clamp((count - MIN_POINTS) / (FULL_POINTS - MIN_POINTS), 0, 1);
  const highF = 0.5 + 0.5 * (high / count);
  // ARKit's depth is good to about 1.5 m and useless for sizing by 3.
  const distF = clamp((3 - distance) / 1.5, 0, 1);
  const spanF = clamp(span / FULL_SPAN_PX, 0, 1);
  // Looking at an item from near its own height shows its front and hides its
  // top, so the depth of the footprint is a guess. Full credit from 35 degrees
  // below the horizon, a floor of 0.3 at 10 and under.
  const view = normalize(centre);
  const up = upVector(frame);
  const below = Math.asin(clamp(-dot(up, view), -1, 1));
  const viewF = clamp(0.3 + (0.7 * (below - toRad(10))) / toRad(25), 0.3, 1);
  const planeF = plane.source === "fit" ? 1 : plane.source === "arkit" ? 0.85 : 0.5;
  const truncF = truncated ? 0.6 : 1;
  const confidence = countF * highF * distF * spanF * viewF * planeF * truncF;

  const dims = [long, short, height].map((m) => roundHalfCm(m * 100)).sort((a, b) => b - a);
  return {
    l: dims[0] as number,
    w: dims[1] as number,
    h: dims[2] as number,
    confidence: Math.round(confidence * 100) / 100,
  };
}

function depthsOf(g: PointGrid, idx: number[]): Float64Array {
  const out = new Float64Array(idx.length);
  for (let q = 0; q < idx.length; q++) out[q] = g.depth[idx[q] as number] as number;
  return out;
}

function toRad(deg: number): number {
  return (deg * Math.PI) / 180;
}

function roundHalfCm(cm: number): number {
  return Math.round(cm * 2) / 2;
}

// ---------------------------------------------------------------------------
// The support surface
// ---------------------------------------------------------------------------

/**
 * The surface the item stands on.
 *
 * The LOWEST dominant peak of heights among the surrounding points, because a
 * region is loose: its padding usually also catches some of the item's own
 * top, and of whatever is beside it, and every one of those is higher than the
 * surface they all stand on. "Dominant" (at least 35% of the biggest peak)
 * keeps the thin tail of noise below the surface from winning.
 *
 * Refined by a least-squares plane through the points within 1.5 cm of that
 * peak, twice, so a surface that is not quite level -- or a gravity estimate
 * that is not quite right -- is followed rather than assumed away. A fit
 * steeper than 15 degrees is not a shelf; the flat peak height is kept then.
 */
function supportPlane(
  frame: DepthFrame,
  g: PointGrid,
  inRing: (i: number, j: number) => boolean,
  inInner: (i: number, j: number) => boolean,
): SupportPlane | null {
  const up = upVector(frame);
  const ring: number[] = [];
  for (let j = 0; j < g.height; j++) {
    for (let i = 0; i < g.width; i++) {
      const k = j * g.width + i;
      if (g.valid[k] && inRing(i, j)) ring.push(k);
    }
  }

  const ringH = ring.map((k) => g.h[k] as number);
  const peaks = histogramPeaks(ringH, { minShare: 0.35, minPoints: 20 });
  const peak = peaks[0];
  if (peak) {
    const [e1, e2] = planeBasis(up);
    let plane: SupportPlane = { n: up, c: peak.height, source: "fit" };
    for (let iter = 0; iter < 2; iter++) {
      const s: number[] = [];
      const t: number[] = [];
      const h: number[] = [];
      for (const k of ring) {
        const p: Vec3 = [g.x[k] as number, g.y[k] as number, g.z[k] as number];
        if (Math.abs(dot(plane.n, p) - plane.c) > PLANE_BAND) continue;
        s.push(dot(e1, p));
        t.push(dot(e2, p));
        h.push(dot(up, p));
      }
      if (h.length < 30) break;
      const fit = fitHeightPlane(s, t, h);
      if (!fit || Math.atan(Math.hypot(fit.a, fit.b)) > MAX_PLANE_TILT) {
        // Too steep to be a support, or degenerate: keep it level, at the
        // mean height of the band.
        plane = { n: up, c: h.reduce((a, b) => a + b, 0) / h.length, source: "fit" };
        break;
      }
      // h - a s - b t = c  <=>  (up - a e1 - b e2) . p = c, normalised.
      const raw: Vec3 = [
        up[0] - fit.a * e1[0] - fit.b * e2[0],
        up[1] - fit.a * e1[1] - fit.b * e2[1],
        up[2] - fit.a * e1[2] - fit.b * e2[2],
      ];
      const norm = Math.hypot(raw[0], raw[1], raw[2]);
      plane = { n: [raw[0] / norm, raw[1] / norm, raw[2] / norm], c: fit.c / norm, source: "fit" };
    }
    return plane;
  }

  // No surface of its own in view: an item filling its region, say, or a
  // black shelf the LiDAR cannot see. ARKit may have found one earlier.
  const inner: number[] = [];
  for (let j = 0; j < g.height; j++) {
    for (let i = 0; i < g.width; i++) {
      const k = j * g.width + i;
      if (g.valid[k] && inInner(i, j)) inner.push(k);
    }
  }
  const arkit = arkitPlaneUnder(frame, g, inner);
  if (arkit) return arkit;

  // Last resort: assume the lowest few percent of what we can see is the
  // floor. Often right for an item on the floor, and marked as a guess.
  const all: number[] = [];
  for (let k = 0; k < g.width * g.height; k++) if (g.valid[k]) all.push(g.h[k] as number);
  if (all.length < MIN_POINTS) return null;
  return { n: up, c: quantile(all, 0.05), source: "percentile" };
}

/**
 * The horizontal ARKit plane most of the region's points stand over, if any.
 *
 * A plane counts when its normal is within 15 degrees of up and the point
 * lies inside its extent (in the plane's own x-z) no more than 2 cm below it.
 * A plane that only a handful of points stand over is not "under the region".
 */
function arkitPlaneUnder(frame: DepthFrame, g: PointGrid, inner: number[]): SupportPlane | null {
  if (inner.length === 0) return null;
  const world = inner.map((k) => cameraToWorld(frame, [g.x[k] as number, g.y[k] as number, g.z[k] as number]));
  let best: { plane: SupportPlane; over: number } | null = null;
  for (const pl of frame.planes) {
    const m = pl.transform;
    const col = (c: number): Vec3 => [m[c * 4] ?? 0, m[c * 4 + 1] ?? 0, m[c * 4 + 2] ?? 0];
    const ax = normalize(col(0));
    const ny = normalize(col(1));
    const az = normalize(col(2));
    const o = col(3);
    if (ny[1] < Math.cos(MAX_PLANE_TILT)) continue;
    let over = 0;
    for (const w of world) {
      const d: Vec3 = [w[0] - o[0], w[1] - o[1], w[2] - o[2]];
      if (dot(d, ny) < -0.02) continue;
      if (Math.abs(dot(d, ax)) > pl.extent[0] / 2 || Math.abs(dot(d, az)) > pl.extent[1] / 2) continue;
      over++;
    }
    if (over < Math.max(MIN_POINTS, inner.length * 0.3)) continue;
    if (best && over <= best.over) continue;
    const n = worldDirToCamera(frame, ny);
    best = { plane: { n, c: dot(n, worldToCamera(frame, o)), source: "arkit" }, over };
  }
  return best?.plane ?? null;
}

// ---------------------------------------------------------------------------
// Connected pieces
// ---------------------------------------------------------------------------

/**
 * 4-connected components of `mask`, with a link between neighbours broken
 * when their depths differ by more than 3 cm. Labels are 1-based; 0 is
 * background. sizes[label] is the pixel count.
 */
function components(g: PointGrid, mask: Uint8Array): { labels: Int32Array; sizes: number[] } {
  const w = g.width;
  const n = w * g.height;
  const labels = new Int32Array(n);
  const sizes: number[] = [0];
  const stack: number[] = [];
  let next = 0;
  for (let start = 0; start < n; start++) {
    if (!mask[start] || labels[start]) continue;
    next++;
    let size = 0;
    labels[start] = next;
    stack.push(start);
    while (stack.length > 0) {
      const k = stack.pop() as number;
      size++;
      const d = g.depth[k] as number;
      const i = k % w;
      const visit = (kk: number) => {
        if (!mask[kk] || labels[kk]) return;
        if (Math.abs((g.depth[kk] as number) - d) > DEPTH_JUMP) return;
        labels[kk] = next;
        stack.push(kk);
      };
      if (i > 0) visit(k - 1);
      if (i < w - 1) visit(k + 1);
      if (k >= w) visit(k - w);
      if (k + w < n) visit(k + w);
    }
    sizes.push(size);
  }
  return { labels, sizes };
}

/** The piece with the most pixels inside the region itself. -1 when none. */
function mostInRegion(
  g: PointGrid,
  labels: Int32Array,
  sizes: number[],
  inInner: (i: number, j: number) => boolean,
): number {
  const inside = new Array<number>(sizes.length).fill(0);
  for (let j = 0; j < g.height; j++) {
    for (let i = 0; i < g.width; i++) {
      const l = labels[j * g.width + i] as number;
      if (l > 0 && inInner(i, j)) inside[l] = (inside[l] as number) + 1;
    }
  }
  let best = -1;
  for (let l = 1; l < sizes.length; l++) {
    if ((inside[l] as number) < MIN_POINTS) continue;
    if (best < 0 || (inside[l] as number) > (inside[best] as number)) best = l;
  }
  return best;
}

/**
 * With no region: the piece that comes closest to the middle of the frame,
 * among those big enough to measure -- which is where a person points the
 * camera. Specks of noise that clear 1.5 cm are never big enough.
 */
function nearestCentre(frame: DepthFrame, g: PointGrid, labels: Int32Array, sizes: number[]): number {
  const cx = frame.depthW / 2 - g.rect.x0;
  const cy = frame.depthH / 2 - g.rect.y0;
  const nearest = new Array<number>(sizes.length).fill(Infinity);
  for (let j = 0; j < g.height; j++) {
    for (let i = 0; i < g.width; i++) {
      const l = labels[j * g.width + i] as number;
      if (l === 0) continue;
      const d = Math.hypot(i + 0.5 - cx, j + 0.5 - cy);
      if (d < (nearest[l] as number)) nearest[l] = d;
    }
  }
  let best = -1;
  for (let l = 1; l < sizes.length; l++) {
    if ((sizes[l] as number) < MIN_POINTS) continue;
    if (best < 0 || (nearest[l] as number) < (nearest[best] as number)) best = l;
  }
  return best;
}
