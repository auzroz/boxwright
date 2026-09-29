// How full an open container is, from one LiDAR frame looking down into it.
//
// The user lines the container's rim up with a guide rectangle on screen and
// we already know its interior size (it is on the container in Homebox, or
// was just measured). So this never has to find the container -- only its rim
// height, and the height of whatever is inside, cell by cell.

import type { Region } from "../types";
import {
  type DepthFrame,
  type PointGrid,
  type Vec3,
  dot,
  isSupportedOrientation,
  padRegion,
  planeBasis,
  pointGrid,
  portraitToSensor,
  sensorRectPixels,
  upVector,
} from "./frame";
import { clamp, histogramPeaks, orientedFootprint, quantile } from "./stats";

/** Centimetres, as stored on the container. */
export interface Interior {
  l: number;
  w: number;
  h: number;
}

export type FillResult = { fillPct: number; overfull: boolean; confidence: number } | { refused: string };

/**
 * Beyond this from straight down, the near wall hides too much of the inside
 * (at 30 degrees, a 30 cm deep container hides a 17 cm strip) for the rest to
 * stand in for it.
 */
const MAX_TILT_DEG = 30;
/** The outer share of the guide rectangle, per side, searched for the rim. */
const RIM_BAND = 0.15;
/** Heights within this of the rim peak are rim. */
const RIM_TOLERANCE = 0.015;
/** Kept clear of the walls, so their inner faces are never read as contents. */
const WALL_INSET = 0.015;
/** Top-down grid cell. */
const CELL = 0.01;
/**
 * Contents level with the rim are full, not overfull. The rim is itself found
 * to within about a centimetre and the max-per-cell grid reads a little high,
 * so "above it" has to mean clearly above it: on synthetic frames, a 34 cm deep
 * container filled exactly to the rim reads a mean of 1.007-1.011 at 0-25
 * degrees of tilt, which a bare "> 1" would call overfull every time.
 */
const OVERFULL_MARGIN = 0.015;

/**
 * Measures how full the container framed by `guideRect` (fractions of the
 * upright photo) is, given its interior size.
 *
 * Returns a refusal, in words for the screen, whenever the frame cannot
 * support an answer -- a wrong fill level written to Homebox is worse than
 * asking for another photo.
 */
export function measureFill(frame: DepthFrame, guideRect: Region, interior: Interior): FillResult {
  if (!(interior.l > 0 && interior.w > 0 && interior.h > 0)) {
    return { refused: "The container's inside size is needed to measure how full it is." };
  }
  if (!isSupportedOrientation(frame.orientation)) return { refused: "This photo cannot be measured." };

  // Straight down is +z in camera space against -up; the angle between them
  // is the tilt.
  const up = upVector(frame);
  // To a tenth of a degree, so exactly 30 is not refused over a rounding error.
  const tiltDeg = Math.round((Math.acos(clamp(-up[2], -1, 1)) * 1800) / Math.PI) / 10;
  if (tiltDeg > MAX_TILT_DEG) {
    return {
      refused: `The phone is tilted ${Math.round(tiltDeg)}° from straight down. Hold it flatter over the container, within ${MAX_TILT_DEG}°.`,
    };
  }

  const L = Math.max(interior.l, interior.w) / 100;
  const W = Math.min(interior.l, interior.w) / 100;
  const H = interior.h / 100;

  const guide = portraitToSensor(guideRect, frame.orientation);
  const guidePx = sensorRectPixels(frame, guide);
  const outerPx = sensorRectPixels(frame, padRegion(guide, 0.25));
  if (guidePx.x1 - guidePx.x0 < 8 || guidePx.y1 - guidePx.y0 < 8) {
    return { refused: "The container is too small in the picture. Move closer." };
  }

  // Raw depth for the rim, which may be only a couple of pixels wide and would
  // not survive a median. Smoothed depth (below) for the contents, which are broad.
  const raw = pointGrid(frame, outerPx, false);
  const gx0 = guidePx.x0 - outerPx.x0;
  const gy0 = guidePx.y0 - outerPx.y0;
  const gw = guidePx.x1 - guidePx.x0;
  const gh = guidePx.y1 - guidePx.y0;
  const bandX = Math.max(1, Math.round(gw * RIM_BAND));
  const bandY = Math.max(1, Math.round(gh * RIM_BAND));
  const inGuide = (i: number, j: number) => i >= gx0 && i < gx0 + gw && j >= gy0 && j < gy0 + gh;
  const inBand = (i: number, j: number) =>
    inGuide(i, j) && (i < gx0 + bandX || i >= gx0 + gw - bandX || j < gy0 + bandY || j >= gy0 + gh - bandY);

  // The rim: the HIGHEST real surface in the edge band. The band also holds
  // the floor outside, the walls' inner faces and the contents near them --
  // all lower than the rim, unless the container is heaped past it at the
  // very edge. The rim is a thin ring, so its peak may be small next to the
  // floor's: anything 3% the height of the biggest peak counts.
  const band = collect(raw, inBand);
  const rimPeaks = histogramPeaks(band.map((k) => raw.h[k] as number), {
    minShare: 0.03,
    minPoints: 12,
  });
  // A rim goes all the way round, so its height turns up on at least three of
  // the band's four sides. Something heaped past the rim near one wall -- or
  // looming larger on the near side, at a tilt -- turns up on one or two, and
  // is passed over for the next peak down.
  const side = (k: number): number => {
    const i = k % raw.width;
    const j = Math.floor(k / raw.width);
    if (i < gx0 + bandX) return 0;
    if (i >= gx0 + gw - bandX) return 1;
    if (j < gy0 + bandY) return 2;
    return 3;
  };
  const rimPeak = [...rimPeaks].reverse().find((peak) => {
    const hits = [0, 0, 0, 0];
    for (const k of band) {
      if (Math.abs((raw.h[k] as number) - peak.height) <= RIM_TOLERANCE) hits[side(k)] = (hits[side(k)] as number) + 1;
    }
    return hits.filter((n) => n >= 3).length >= 3;
  });
  if (!rimPeak) return { refused: "Could not find the container's rim. Line its top edge up with the guide." };

  const rimPts = band.filter((k) => Math.abs((raw.h[k] as number) - rimPeak.height) <= RIM_TOLERANCE);
  if (rimPts.length < 20) {
    return { refused: "Could not find the container's rim. Line its top edge up with the guide." };
  }
  const rim = rimPts.reduce((a, k) => a + (raw.h[k] as number), 0) / rimPts.length;
  const bottom = rim - H;

  // The floor around it, when in view, cross-checks the rim: a container
  // stands its interior depth plus a floor and some feet (0-5 cm) tall.
  // Agreement raises confidence; a clear disagreement means the rim peak is
  // probably something else -- a lid, a shelf -- and lowers it.
  const outside = collect(raw, (i, j) => !inGuide(i, j));
  const floorPeak = histogramPeaks(outside.map((k) => raw.h[k] as number), { minShare: 0.35, minPoints: 30 })[0];
  // A "rim" level with the floor around it is the floor: the guide is on
  // nothing, and every cell would read as full to the brim.
  if (floorPeak && rim - floorPeak.height < 0.05) {
    return { refused: "Could not find the container's rim. Line its top edge up with the guide." };
  }
  let floorF = 0.85;
  if (floorPeak && floorPeak.height < rim - H / 2) {
    const stands = rim - floorPeak.height;
    floorF = stands >= H - 0.015 && stands <= H + 0.065 ? 1 : 0.5;
  }

  // Where the inside is: the rim points' own trimmed rectangle gives the
  // centre and the orientation. Its midpoint, not the points' mean, because
  // tilt puts more pixels on the near side of the rim than the far.
  const [e1, e2] = planeBasis(up);
  const rs = rimPts.map((k) => dot(e1, pointOf(raw, k)));
  const rt = rimPts.map((k) => dot(e2, pointOf(raw, k)));
  const foot = orientedFootprint(rs, rt, 0.02, 0.98);
  const hl = L / 2 - WALL_INSET;
  const hw = W / 2 - WALL_INSET;
  if (hl <= CELL || hw <= CELL) return { refused: "The container is too small inside to measure." };

  // The rim should span about the inside size (plus two walls' thickness).
  // Much smaller or larger means the guide is on something else.
  const extentRatio = Math.min(foot.long / L, foot.short / W, L / foot.long, W / foot.short);
  const extentF = clamp((extentRatio - 0.6) / 0.25, 0, 1);

  // Top-down 1 cm grid of the inside, each cell the HIGHEST reading in it: at
  // a tilt a cell also catches the side of whatever is heaped in it, and the
  // top is what fills the container.
  const smooth = pointGrid(frame, outerPx, true);
  const nx = Math.max(1, Math.floor((2 * hl) / CELL));
  const ny = Math.max(1, Math.floor((2 * hw) / CELL));
  const grid = new Float64Array(nx * ny).fill(NaN);
  for (let k = 0; k < smooth.width * smooth.height; k++) {
    if (!smooth.valid[k] || onEdge(smooth, k)) continue;
    const p = pointOf(smooth, k);
    const ds = dot(e1, p) - foot.cs;
    const dt = dot(e2, p) - foot.ct;
    const a = ds * foot.us + dt * foot.ut; // along the long side
    const b = -ds * foot.ut + dt * foot.us;
    if (Math.abs(a) >= hl || Math.abs(b) >= hw) continue;
    const cx = Math.min(nx - 1, Math.floor((a + hl) / CELL));
    const cy = Math.min(ny - 1, Math.floor((b + hw) / CELL));
    const cell = cy * nx + cx;
    const h = smooth.h[k] as number;
    const prev = grid[cell] as number;
    if (Number.isNaN(prev) || h > prev) grid[cell] = h;
  }

  let seen = 0;
  for (let c = 0; c < grid.length; c++) if (!Number.isNaN(grid[c] as number)) seen++;
  const seenShare = seen / grid.length;
  if (seenShare < 0.25) {
    return { refused: "Too little of the inside was visible. Hold the phone over the middle of the container." };
  }
  fillGaps(grid, nx, ny);

  let sum = 0;
  for (let c = 0; c < grid.length; c++) sum += clamp(((grid[c] as number) - bottom) / H, 0, 1.2);
  const mean = sum / grid.length;

  // Depth to the rim, for the usual LiDAR range fall-off.
  const distance = quantile(
    rimPts.map((k) => raw.depth[k] as number),
    0.5,
  );
  const distF = clamp((3 - distance) / 1.5, 0, 1);
  const tiltF = tiltDeg <= 20 ? 1 : 1 - (0.3 * (tiltDeg - 20)) / (MAX_TILT_DEG - 20);
  const confidence = clamp(seenShare, 0, 1) * extentF * floorF * distF * tiltF;

  return {
    fillPct: clamp(Math.round(mean * 100), 0, 100),
    overfull: mean > 1 + OVERFULL_MARGIN / H,
    confidence: Math.round(confidence * 100) / 100,
  };
}

function pointOf(g: PointGrid, k: number): Vec3 {
  return [g.x[k] as number, g.y[k] as number, g.z[k] as number];
}

/**
 * Whether a pixel sits on a depth edge: a 4-neighbour more than 3 cm nearer
 * or further. Looking over the near wall, the pixels just past the rim are
 * where the LiDAR interpolates between rim and floor -- points hanging in the
 * air over the inside, which a max-per-cell grid would read as contents. They
 * form a continuous line, so a 3x3 median does not remove them; an edge test
 * does, and costs only samples the gap filling replaces.
 */
function onEdge(g: PointGrid, k: number): boolean {
  const d = g.depth[k] as number;
  const i = k % g.width;
  const near = (kk: number) => g.valid[kk] === 1 && Math.abs((g.depth[kk] as number) - d) > 0.03;
  return (
    (i > 0 && near(k - 1)) ||
    (i < g.width - 1 && near(k + 1)) ||
    (k >= g.width && near(k - g.width)) ||
    (k + g.width < g.width * g.height && near(k + g.width))
  );
}

function collect(g: PointGrid, keep: (i: number, j: number) => boolean): number[] {
  const out: number[] = [];
  for (let j = 0; j < g.height; j++) {
    for (let i = 0; i < g.width; i++) {
      const k = j * g.width + i;
      if (g.valid[k] && keep(i, j)) out.push(k);
    }
  }
  return out;
}

/**
 * Fills cells nobody saw -- the strip behind the near wall, holes -- with the
 * median of their seen neighbours, growing inward a ring at a time. Each pass
 * reads only the previous pass's values, so the fill does not run
 * preferentially in the direction the loop happens to scan.
 */
function fillGaps(grid: Float64Array, nx: number, ny: number): void {
  const window: number[] = [];
  for (let pass = 0; pass < nx + ny; pass++) {
    const prev = grid.slice();
    let missing = 0;
    for (let y = 0; y < ny; y++) {
      for (let x = 0; x < nx; x++) {
        const c = y * nx + x;
        if (!Number.isNaN(prev[c] as number)) continue;
        window.length = 0;
        for (let dy = -1; dy <= 1; dy++) {
          for (let dx = -1; dx <= 1; dx++) {
            const xx = x + dx;
            const yy = y + dy;
            if (xx < 0 || yy < 0 || xx >= nx || yy >= ny) continue;
            const v = prev[yy * nx + xx] as number;
            if (!Number.isNaN(v)) window.push(v);
          }
        }
        if (window.length === 0) {
          missing++;
          continue;
        }
        window.sort((p, q) => p - q);
        grid[c] = window[window.length >> 1] as number;
      }
    }
    if (missing === 0) return;
  }
}
