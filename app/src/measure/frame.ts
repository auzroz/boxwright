// One LiDAR depth frame, and the geometry that turns its pixels into points.
//
// Pure TypeScript with no React Native import, so every line of the maths
// below runs under jest against synthetic scenes (app/test/synthdepth.ts) and
// the device is needed only to confirm the conventions, not to debug them.
//
// Three coordinate systems meet here, and mixing them up is the single most
// likely way for this code to be quietly wrong. They are:
//
// SENSOR image: what ARKit hands over. capturedImage and sceneDepth are both
//   in the camera sensor's native landscape orientation, whatever way up the
//   phone is held. Pixel (u, v): u right, v down, origin top-left; a pixel's
//   centre is at (i + 0.5, j + 0.5). The intrinsics are in capturedImage
//   pixels (e.g. 1920x1440); the depth map is a lower resolution (256x192) of
//   the same field of view, so they are scaled, never re-derived.
//
// PORTRAIT image: the upright JPEG the user (and the vision model) sees.
//   Regions (types.Region) are fractions of THIS image. For a phone held
//   upright the JPEG is the sensor image turned 90 degrees clockwise, which is
//   EXIF orientation 6, UIImage.Orientation `.right`.
//
// CAMERA space, the pinhole convention used for every point in this module:
//   x right and y down in the SENSOR image, z forward out of the lens; a
//   point's z is its depth. ARKit's own camera space is x right, y UP, z
//   BACKWARD (the camera looks down -z), so a point converts as
//   (x, -y, -z). Heights are measured along `up`, gravity's opposite,
//   expressed in this camera space -- so "height" is relative to the camera
//   and negative for anything below it. Only differences of height mean
//   anything.

import type { Region } from "../types";

export type Vec3 = [number, number, number];

/** A plane ARKit detected, as it was when the frame was captured. */
export interface DepthPlane {
  /**
   * Anchor-to-world, 4x4 COLUMN-major (simd_float4x4 memory order: m[col*4 +
   * row], translation in m[12..14]). The plane lies in the anchor's x-z plane
   * with its normal along the anchor's +y. The native side bakes the plane's
   * centre offset into this transform, so the extent is centred on it.
   */
  transform: number[];
  /** Width along the anchor's x, and depth along its z, in metres. */
  extent: [number, number];
}

/** A decoded `.depth` file (see depthfile.ts for the byte layout). */
export interface DepthFrame {
  /** Depth map size, in SENSOR orientation (e.g. 256x192). */
  depthW: number;
  depthH: number;
  /** capturedImage size, the pixels the intrinsics are expressed in. */
  imageW: number;
  imageH: number;
  /** 3x3 ROW-major: [fx, 0, cx, 0, fy, cy, 0, 0, 1], in imageW x imageH pixels. */
  intrinsics: number[];
  /** ARKit camera-to-world, 4x4 COLUMN-major. ARKit's world is gravity-aligned: +y is up. */
  cameraTransform: number[];
  /** EXIF orientation of the upright photo relative to the sensor: 1, 3, 6 or 8. */
  orientation: number;
  planes: DepthPlane[];
  /** Depth along z in millimetres, row-major, depthW x depthH. 0 = no reading. */
  depthMm: Uint16Array;
  /** ARKit ARConfidenceLevel per pixel: 0 low, 1 medium, 2 high. */
  confidence: Uint8Array;
}

/**
 * The working range. Under 15 cm the LiDAR has nothing to say; past 4 m the
 * readings are too coarse to size a thing on a shelf, and ARKit's own
 * confidence falls away there too.
 */
export const MIN_DEPTH_M = 0.15;
export const MAX_DEPTH_M = 4;

/** The EXIF orientations a back camera produces. Mirrored ones (2, 4, 5, 7) never occur. */
export const ORIENTATION_UP = 1;
export const ORIENTATION_DOWN = 3;
export const ORIENTATION_RIGHT = 6;
export const ORIENTATION_LEFT = 8;

export function isSupportedOrientation(o: number): boolean {
  return o === ORIENTATION_UP || o === ORIENTATION_DOWN || o === ORIENTATION_RIGHT || o === ORIENTATION_LEFT;
}

// ---------------------------------------------------------------------------
// Orientation: portrait (displayed) fractions <-> sensor fractions
// ---------------------------------------------------------------------------

/**
 * Maps a region of the UPRIGHT photo onto the sensor image, both as fractions.
 *
 * For `.right` (6), the upright photo is the sensor image rotated 90 degrees
 * clockwise: sensor pixel (xs, ys) is displayed at (1 - ys, xs). So a portrait
 * point goes back as xs = yp, ys = 1 - xp, and a rectangle -- whose far corner
 * moves too -- as
 *
 *     xs = y,  ys = 1 - x - w,  ws = h,  hs = w.
 *
 * `.left` (8) is the opposite turn, `.down` (3) a half turn, `.up` (1) none.
 * Throws on any other value: a wrong mapping measures the wrong object, and
 * that must never be a silent default.
 */
export function portraitToSensor(r: Region, orientation: number): Region {
  switch (orientation) {
    case ORIENTATION_UP:
      return { ...r };
    case ORIENTATION_DOWN:
      return { x: 1 - r.x - r.w, y: 1 - r.y - r.h, w: r.w, h: r.h };
    case ORIENTATION_RIGHT:
      return { x: r.y, y: 1 - r.x - r.w, w: r.h, h: r.w };
    case ORIENTATION_LEFT:
      return { x: 1 - r.y - r.h, y: r.x, w: r.h, h: r.w };
    default:
      throw new Error(`unsupported image orientation ${orientation}`);
  }
}

/** The inverse of portraitToSensor. */
export function sensorToPortrait(r: Region, orientation: number): Region {
  switch (orientation) {
    case ORIENTATION_UP:
      return { ...r };
    case ORIENTATION_DOWN:
      return { x: 1 - r.x - r.w, y: 1 - r.y - r.h, w: r.w, h: r.h };
    case ORIENTATION_RIGHT:
      return { x: 1 - r.y - r.h, y: r.x, w: r.h, h: r.w };
    case ORIENTATION_LEFT:
      return { x: r.y, y: 1 - r.x - r.w, w: r.h, h: r.w };
    default:
      throw new Error(`unsupported image orientation ${orientation}`);
  }
}

/** A half-open rectangle of depth pixels: x0 <= i < x1, y0 <= j < y1. */
export interface PixelRect {
  x0: number;
  y0: number;
  x1: number;
  y1: number;
}

/** A sensor-fraction region as depth pixels, clamped to the map. Empty when it misses. */
export function sensorRectPixels(frame: DepthFrame, r: Region): PixelRect {
  const clampX = (v: number) => Math.min(frame.depthW, Math.max(0, v));
  const clampY = (v: number) => Math.min(frame.depthH, Math.max(0, v));
  return {
    x0: clampX(Math.floor(r.x * frame.depthW)),
    y0: clampY(Math.floor(r.y * frame.depthH)),
    x1: clampX(Math.ceil((r.x + r.w) * frame.depthW)),
    y1: clampY(Math.ceil((r.y + r.h) * frame.depthH)),
  };
}

/** Grows a region by `pad` of its own size on every side (0.25 -> 1.5x as wide). */
export function padRegion(r: Region, pad: number): Region {
  return { x: r.x - r.w * pad, y: r.y - r.h * pad, w: r.w * (1 + 2 * pad), h: r.h * (1 + 2 * pad) };
}

// ---------------------------------------------------------------------------
// Back-projection
// ---------------------------------------------------------------------------

/** Pinhole intrinsics at DEPTH-map resolution. */
export interface Intrinsics {
  fx: number;
  fy: number;
  cx: number;
  cy: number;
}

/**
 * The intrinsics scaled from capturedImage pixels down to depth pixels.
 *
 * The depth map covers the same field of view at a lower resolution, so a
 * plain scale is exact; both axes are scaled separately in case a future
 * device's aspect ratios differ.
 */
export function depthIntrinsics(frame: DepthFrame): Intrinsics {
  const k = frame.intrinsics;
  const sx = frame.depthW / frame.imageW;
  const sy = frame.depthH / frame.imageH;
  return { fx: (k[0] ?? 0) * sx, fy: (k[4] ?? 0) * sy, cx: (k[2] ?? 0) * sx, cy: (k[5] ?? 0) * sy };
}

/** Depth pixel (u, v) at `depth` metres, as a camera-space point. u, v are pixel-centre coordinates. */
export function backProject(k: Intrinsics, u: number, v: number, depth: number): Vec3 {
  return [((u - k.cx) * depth) / k.fx, ((v - k.cy) * depth) / k.fy, depth];
}

/** Camera-space point -> depth pixel coordinates, or null behind the camera. */
export function project(k: Intrinsics, p: Vec3): [number, number] | null {
  if (!(p[2] > 1e-6)) return null;
  return [(p[0] / p[2]) * k.fx + k.cx, (p[1] / p[2]) * k.fy + k.cy];
}

// ---------------------------------------------------------------------------
// Camera <-> world
// ---------------------------------------------------------------------------

/** Element (row, col) of a column-major 4x4. */
function m4(m: number[], row: number, col: number): number {
  return m[col * 4 + row] ?? 0;
}

/**
 * World +y (gravity's opposite) in camera space.
 *
 * The camera-to-world rotation R maps camera axes into the world, so world +y
 * seen from the camera is R^T (0, 1, 0) -- R's second ROW. That is in ARKit's
 * camera axes; flipping y and z gives ours. For a phone held upright and level,
 * this comes out as (-1, 0, 0): ARKit's camera x runs along the sensor's long
 * side, which in portrait points at the floor.
 */
export function upVector(frame: DepthFrame): Vec3 {
  const t = frame.cameraTransform;
  const ux = m4(t, 1, 0);
  const uy = m4(t, 1, 1);
  const uz = m4(t, 1, 2);
  const n = Math.hypot(ux, uy, uz) || 1;
  return [ux / n, -uy / n, -uz / n];
}

/** A camera-space point in ARKit world coordinates. */
export function cameraToWorld(frame: DepthFrame, p: Vec3): Vec3 {
  const t = frame.cameraTransform;
  const a: Vec3 = [p[0], -p[1], -p[2]]; // our axes -> ARKit camera axes
  const out: Vec3 = [0, 0, 0];
  for (let r = 0; r < 3; r++) {
    out[r] = m4(t, r, 0) * a[0] + m4(t, r, 1) * a[1] + m4(t, r, 2) * a[2] + m4(t, r, 3);
  }
  return out;
}

/** An ARKit world point in camera space. Assumes a rigid transform, which ARKit's is. */
export function worldToCamera(frame: DepthFrame, w: Vec3): Vec3 {
  const t = frame.cameraTransform;
  const d: Vec3 = [w[0] - m4(t, 0, 3), w[1] - m4(t, 1, 3), w[2] - m4(t, 2, 3)];
  // R^T d, then ARKit camera axes -> ours.
  const a: Vec3 = [0, 0, 0];
  for (let c = 0; c < 3; c++) a[c] = m4(t, 0, c) * d[0] + m4(t, 1, c) * d[1] + m4(t, 2, c) * d[2];
  return [a[0], -a[1], -a[2]];
}

/** A world DIRECTION in camera space (rotation only). */
export function worldDirToCamera(frame: DepthFrame, w: Vec3): Vec3 {
  const t = frame.cameraTransform;
  const a: Vec3 = [0, 0, 0];
  for (let c = 0; c < 3; c++) a[c] = m4(t, 0, c) * w[0] + m4(t, 1, c) * w[1] + m4(t, 2, c) * w[2];
  return [a[0], -a[1], -a[2]];
}

// ---------------------------------------------------------------------------
// Points
// ---------------------------------------------------------------------------

/**
 * Every usable depth pixel inside a rectangle, as camera-space points with a
 * height along gravity. Structure-of-arrays, indexed (j - y0) * width + (i - x0),
 * because the callers walk neighbours on the pixel grid.
 */
export interface PointGrid {
  rect: PixelRect;
  width: number;
  height: number;
  /** Whether the pixel had a usable reading (confidence >= 1, in range). */
  valid: Uint8Array;
  depth: Float32Array;
  x: Float32Array;
  y: Float32Array;
  z: Float32Array;
  /** up . p -- height relative to the camera, in metres. */
  h: Float32Array;
  conf: Uint8Array;
}

function median(values: number[]): number {
  values.sort((a, b) => a - b);
  const n = values.length;
  const mid = n >> 1;
  return n % 2 === 1 ? (values[mid] as number) : ((values[mid - 1] as number) + (values[mid] as number)) / 2;
}

/**
 * Builds the point grid for a rectangle.
 *
 * `smooth` runs a 3x3 median over the depth first, among usable pixels only.
 * A median keeps a depth edge where it is (unlike a mean, which smears the box
 * into the floor) while removing the lone flying pixels ARKit leaves along
 * such edges and roughly halving the per-pixel noise. It is NOT for anything
 * one or two pixels wide -- a container's rim would vanish in it -- which is
 * why it is a choice rather than a default.
 */
export function pointGrid(frame: DepthFrame, rect: PixelRect, smooth: boolean): PointGrid {
  const width = Math.max(0, rect.x1 - rect.x0);
  const height = Math.max(0, rect.y1 - rect.y0);
  const n = width * height;
  const valid = new Uint8Array(n);
  const raw = new Float32Array(n);
  const conf = new Uint8Array(n);
  for (let j = 0; j < height; j++) {
    for (let i = 0; i < width; i++) {
      const src = (rect.y0 + j) * frame.depthW + rect.x0 + i;
      const d = (frame.depthMm[src] ?? 0) / 1000;
      const c = frame.confidence[src] ?? 0;
      const k = j * width + i;
      conf[k] = c;
      if (c >= 1 && d >= MIN_DEPTH_M && d <= MAX_DEPTH_M) {
        valid[k] = 1;
        raw[k] = d;
      }
    }
  }

  let depth = raw;
  if (smooth) {
    depth = new Float32Array(n);
    const window: number[] = [];
    for (let j = 0; j < height; j++) {
      for (let i = 0; i < width; i++) {
        const k = j * width + i;
        if (!valid[k]) continue;
        window.length = 0;
        for (let dj = -1; dj <= 1; dj++) {
          const jj = j + dj;
          if (jj < 0 || jj >= height) continue;
          for (let di = -1; di <= 1; di++) {
            const ii = i + di;
            if (ii < 0 || ii >= width) continue;
            const kk = jj * width + ii;
            if (valid[kk]) window.push(raw[kk] as number);
          }
        }
        depth[k] = median(window);
      }
    }
  }

  const k = depthIntrinsics(frame);
  const up = upVector(frame);
  const x = new Float32Array(n);
  const y = new Float32Array(n);
  const z = new Float32Array(n);
  const h = new Float32Array(n);
  for (let j = 0; j < height; j++) {
    for (let i = 0; i < width; i++) {
      const idx = j * width + i;
      if (!valid[idx]) continue;
      const d = depth[idx] as number;
      const p = backProject(k, rect.x0 + i + 0.5, rect.y0 + j + 0.5, d);
      x[idx] = p[0];
      y[idx] = p[1];
      z[idx] = p[2];
      h[idx] = up[0] * p[0] + up[1] * p[1] + up[2] * p[2];
    }
  }
  return { rect, width, height, valid, depth, x, y, z, h, conf };
}

// ---------------------------------------------------------------------------
// Small vector helpers shared by the measurements
// ---------------------------------------------------------------------------

export function dot(a: Vec3, b: Vec3): number {
  return a[0] * b[0] + a[1] * b[1] + a[2] * b[2];
}

export function cross(a: Vec3, b: Vec3): Vec3 {
  return [a[1] * b[2] - a[2] * b[1], a[2] * b[0] - a[0] * b[2], a[0] * b[1] - a[1] * b[0]];
}

export function normalize(a: Vec3): Vec3 {
  const n = Math.hypot(a[0], a[1], a[2]) || 1;
  return [a[0] / n, a[1] / n, a[2] / n];
}

/**
 * Two unit vectors perpendicular to `n` and to each other. Which pair does not
 * matter to any caller -- every footprint is re-oriented by its own points.
 */
export function planeBasis(n: Vec3): [Vec3, Vec3] {
  // Seed with whichever camera axis is least parallel to n.
  const ax = Math.abs(n[0]);
  const ay = Math.abs(n[1]);
  const az = Math.abs(n[2]);
  const seed: Vec3 = ax <= ay && ax <= az ? [1, 0, 0] : ay <= az ? [0, 1, 0] : [0, 0, 1];
  const e1 = normalize(cross(n, seed));
  const e2 = normalize(cross(n, e1));
  return [e1, e2];
}
