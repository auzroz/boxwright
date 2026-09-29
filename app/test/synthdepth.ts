/**
 * A synthetic LiDAR frame: a seeded ray-caster over a ground plane, boxes and
 * open-topped containers, with the faults real depth has.
 *
 * It exists so the measurement maths can be graded against KNOWN sizes, which
 * a real capture never gives you. It builds frames exactly the way the native
 * side will -- ARKit axes, portrait orientation `.right`, intrinsics in
 * capturedImage pixels, depth in sensor orientation -- so it is also the
 * executable statement of those conventions (see src/measure/frame.ts).
 *
 * The faults, and why each is here:
 *  - gaussian noise, sigma = 5 mm + 0.5% of depth: the per-pixel scatter of
 *    iPhone LiDAR at arm's length, and what percentiles have to be robust to;
 *  - flying pixels: depth interpolated part-way across an edge, which is what
 *    ARKit's smoothing does where a box meets the floor behind it;
 *  - holes: pixels with no reading at all;
 *  - lower confidence at edges and far away, as ARKit reports it.
 * Deterministic for a given seed, so a failure is reproducible.
 */
import type { DepthFrame, Vec3 } from "../src/measure/frame";
import { ORIENTATION_RIGHT, depthIntrinsics, project, sensorToPortrait, worldToCamera } from "../src/measure/frame";
import type { Region } from "../src/types";

/** A solid box standing on the ground (or on `base`), turned `yawDeg` about vertical. */
export interface SynthBox {
  /** Centre of its footprint, world x and z (metres). */
  at: [number, number];
  /** Size along its own x, up, and its own z (metres). */
  size: [number, number, number];
  yawDeg?: number;
  /** Height of its underside above the ground. */
  base?: number;
}

/**
 * An open-topped container: four walls and a floor, turned `yawDeg`, holding
 * layers of contents. Interior dimensions are the outer ones less the walls.
 */
export interface SynthContainer {
  at: [number, number];
  /** Outer length (own x), height, width (own z). */
  outer: [number, number, number];
  wall: number;
  floor: number;
  yawDeg?: number;
  /**
   * Contents as flat-topped layers from the interior floor up to `top` metres,
   * each covering the interior shrunk by `inset` (a fraction per side). A heap
   * is several layers, narrower as they go up.
   */
  contents?: { top: number; inset: number }[];
}

export interface SynthScene {
  boxes?: SynthBox[];
  containers?: SynthContainer[];
  /** False for a floor the LiDAR cannot see (black, glossy): no ground hits. */
  ground?: boolean;
}

/** A phone held upright in portrait, looking `pitchDeg` below the horizon, facing `yawDeg`. */
export interface SynthCamera {
  position: Vec3;
  pitchDeg: number;
  yawDeg: number;
}

export interface SynthOptions {
  seed?: number;
  /** Multiplies the noise sigma; 0 for a clean frame. */
  noise?: number;
  /** Share of pixels with no reading. */
  holes?: number;
  /** Chance an edge pixel is a flying pixel. */
  flying?: number;
  /** Include the ground as an ARKit plane anchor (default true, even when `ground` is false). */
  groundAnchor?: boolean;
}

/** The capture geometry of an iPhone wide camera under ARKit, near enough. */
export const IMAGE_W = 1920;
export const IMAGE_H = 1440;
export const DEPTH_W = 256;
export const DEPTH_H = 192;
const FOCAL = 1450;

/** mulberry32: small, fast, and the same sequence on every machine. */
export function prng(seed: number): () => number {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

function gaussian(rand: () => number): number {
  const u = Math.max(rand(), 1e-12);
  return Math.sqrt(-2 * Math.log(u)) * Math.cos(2 * Math.PI * rand());
}

const rad = (d: number) => (d * Math.PI) / 180;

/**
 * The camera `distance` metres from `target`, looking at it from `pitchDeg`
 * below the horizon, facing `yawDeg` (0 = looking along world -z).
 */
export function lookAt(target: Vec3, distance: number, pitchDeg: number, yawDeg: number): SynthCamera {
  const f = forward(pitchDeg, yawDeg);
  return {
    position: [target[0] - f[0] * distance, target[1] - f[1] * distance, target[2] - f[2] * distance],
    pitchDeg,
    yawDeg,
  };
}

function forward(pitchDeg: number, yawDeg: number): Vec3 {
  const p = rad(pitchDeg);
  const y = rad(yawDeg);
  return [-Math.sin(y) * Math.cos(p), -Math.sin(p), -Math.cos(y) * Math.cos(p)];
}

/**
 * ARKit camera-to-world, column-major, for a phone held upright in portrait.
 *
 * ARKit's camera axes are fixed to the SENSOR, whose long side is its x: held
 * in portrait, camera +x points down the displayed image, +y points to its
 * right, and +z back out of the screen towards the user.
 */
export function cameraTransform(cam: SynthCamera): number[] {
  const f = forward(cam.pitchDeg, cam.yawDeg);
  const p = rad(cam.pitchDeg);
  const y = rad(cam.yawDeg);
  const h: Vec3 = [-Math.sin(y), 0, -Math.cos(y)];
  // The displayed image's up: world up tipped with the camera.
  const imageUp: Vec3 = [Math.sin(p) * h[0], Math.cos(p), Math.sin(p) * h[2]];
  const xAr: Vec3 = [-imageUp[0], -imageUp[1], -imageUp[2]];
  // image right = f x imageUp
  const yAr: Vec3 = [
    f[1] * imageUp[2] - f[2] * imageUp[1],
    f[2] * imageUp[0] - f[0] * imageUp[2],
    f[0] * imageUp[1] - f[1] * imageUp[0],
  ];
  const zAr: Vec3 = [-f[0], -f[1], -f[2]];
  const o = cam.position;
  return [...xAr, 0, ...yAr, 0, ...zAr, 0, o[0], o[1], o[2], 1];
}

/** An oriented solid box in world space, as the ray-caster wants it. */
interface Solid {
  c: Vec3; // centre
  half: Vec3;
  cos: number;
  sin: number;
}

function solid(at: [number, number], base: number, size: [number, number, number], yawDeg: number): Solid {
  return {
    c: [at[0], base + size[1] / 2, at[1]],
    half: [size[0] / 2, size[1] / 2, size[2] / 2],
    cos: Math.cos(rad(yawDeg)),
    sin: Math.sin(rad(yawDeg)),
  };
}

/** A point in a yawed frame centred at (cx, cz) -> world. */
function yawed(at: [number, number], yawDeg: number, lx: number, lz: number): [number, number] {
  const c = Math.cos(rad(yawDeg));
  const s = Math.sin(rad(yawDeg));
  return [at[0] + c * lx + s * lz, at[1] - s * lx + c * lz];
}

/** Every solid in the scene: boxes as they are, containers as walls, floor and contents. */
export function solids(scene: SynthScene): Solid[] {
  const out: Solid[] = [];
  for (const b of scene.boxes ?? []) out.push(solid(b.at, b.base ?? 0, b.size, b.yawDeg ?? 0));
  for (const c of scene.containers ?? []) {
    const yaw = c.yawDeg ?? 0;
    const [L, H, W] = c.outer;
    const t = c.wall;
    out.push(solid(yawed(c.at, yaw, -(L - t) / 2, 0), 0, [t, H, W], yaw));
    out.push(solid(yawed(c.at, yaw, (L - t) / 2, 0), 0, [t, H, W], yaw));
    out.push(solid(yawed(c.at, yaw, 0, -(W - t) / 2), 0, [L - 2 * t, H, t], yaw));
    out.push(solid(yawed(c.at, yaw, 0, (W - t) / 2), 0, [L - 2 * t, H, t], yaw));
    out.push(solid(c.at, 0, [L - 2 * t, c.floor, W - 2 * t], yaw));
    const iL = L - 2 * t;
    const iW = W - 2 * t;
    for (const layer of c.contents ?? []) {
      if (layer.top <= 0) continue;
      out.push(solid(c.at, c.floor, [iL * (1 - 2 * layer.inset), layer.top, iW * (1 - 2 * layer.inset)], yaw));
    }
  }
  return out;
}

/**
 * Ray (o + t d) against an oriented box; the entry t, or Infinity. Scalars
 * only: this runs about 250 000 times a frame.
 */
function hitSolid(o: Vec3, d: Vec3, b: Solid): number {
  // Into the box's own frame: translate, then undo the yaw.
  const rx = o[0] - b.c[0];
  const rz = o[2] - b.c[2];
  let t0 = -Infinity;
  let t1 = Infinity;
  const slab = (oa: number, da: number, ha: number): boolean => {
    if (Math.abs(da) < 1e-12) return oa >= -ha && oa <= ha;
    let ta = (-ha - oa) / da;
    let tb = (ha - oa) / da;
    if (ta > tb) {
      const x = ta;
      ta = tb;
      tb = x;
    }
    if (ta > t0) t0 = ta;
    if (tb < t1) t1 = tb;
    return t0 <= t1;
  };
  if (!slab(b.cos * rx - b.sin * rz, b.cos * d[0] - b.sin * d[2], b.half[0])) return Infinity;
  if (!slab(o[1] - b.c[1], d[1], b.half[1])) return Infinity;
  if (!slab(b.sin * rx + b.cos * rz, b.sin * d[0] + b.cos * d[2], b.half[2])) return Infinity;
  return t0 > 1e-6 ? t0 : Infinity;
}

const NEIGHBOURS = [
  [1, 0],
  [-1, 0],
  [0, 1],
  [0, -1],
] as const;

/** Renders the scene as the native module would write it. */
export function renderDepth(scene: SynthScene, cam: SynthCamera, opts: SynthOptions = {}): DepthFrame {
  const rand = prng(opts.seed ?? 1);
  const noise = opts.noise ?? 1;
  const holes = opts.holes ?? 0.02;
  const flying = opts.flying ?? 0.3;
  const transform = cameraTransform(cam);
  const intrinsics = [FOCAL, 0, IMAGE_W / 2, 0, FOCAL, IMAGE_H / 2, 0, 0, 1];
  const frame: DepthFrame = {
    depthW: DEPTH_W,
    depthH: DEPTH_H,
    imageW: IMAGE_W,
    imageH: IMAGE_H,
    intrinsics,
    cameraTransform: transform,
    orientation: ORIENTATION_RIGHT,
    // ARKit finds a floor from the camera image as well as from depth, so a
    // floor the LiDAR cannot see can still have an anchor.
    planes:
      opts.groundAnchor === false
        ? []
        : [{ transform: [1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1], extent: [6, 6] }],
    depthMm: new Uint16Array(DEPTH_W * DEPTH_H),
    confidence: new Uint8Array(DEPTH_W * DEPTH_H),
  };

  const k = depthIntrinsics(frame);
  const col = (c: number): Vec3 => [transform[c * 4] ?? 0, transform[c * 4 + 1] ?? 0, transform[c * 4 + 2] ?? 0];
  const [xAr, yAr, zAr] = [col(0), col(1), col(2)];
  const o = cam.position;
  const list = solids(scene);

  // 1. Clean depth along z.
  const clean = new Float64Array(DEPTH_W * DEPTH_H);
  for (let j = 0; j < DEPTH_H; j++) {
    for (let i = 0; i < DEPTH_W; i++) {
      const dx = (i + 0.5 - k.cx) / k.fx;
      const dy = (j + 0.5 - k.cy) / k.fy;
      // Our camera axes (dx, dy, 1) -> ARKit's (dx, -dy, -1) -> world. With z = 1
      // along the ray, the ray parameter IS the depth.
      const d: Vec3 = [
        xAr[0] * dx - yAr[0] * dy - zAr[0],
        xAr[1] * dx - yAr[1] * dy - zAr[1],
        xAr[2] * dx - yAr[2] * dy - zAr[2],
      ];
      let t = Infinity;
      if (scene.ground !== false && d[1] < -1e-9) t = -o[1] / d[1];
      for (const s of list) t = Math.min(t, hitSolid(o, d, s));
      clean[j * DEPTH_W + i] = Number.isFinite(t) ? t : 0;
    }
  }

  // 2. Faults, then quantise to millimetres as the file does.
  for (let j = 0; j < DEPTH_H; j++) {
    for (let i = 0; i < DEPTH_W; i++) {
      const idx = j * DEPTH_W + i;
      let d = clean[idx] as number;
      if (d <= 0) continue;
      let conf = d > 2.5 ? 1 : 2;
      // An edge: a 4-neighbour at a clearly different depth.
      let other = 0;
      for (const [di, dj] of NEIGHBOURS) {
        const ii = i + di;
        const jj = j + dj;
        if (ii < 0 || jj < 0 || ii >= DEPTH_W || jj >= DEPTH_H) continue;
        const nd = clean[jj * DEPTH_W + ii] as number;
        if (nd > 0 && Math.abs(nd - d) > 0.05) other = nd;
      }
      if (other > 0) {
        conf = 1;
        if (rand() < flying) d = d + (other - d) * rand();
      }
      if (rand() < 0.05) conf = Math.min(conf, 1);
      d += gaussian(rand) * noise * (0.005 + 0.005 * d);
      if (rand() < holes) continue;
      frame.depthMm[idx] = Math.max(0, Math.min(65535, Math.round(d * 1000)));
      frame.confidence[idx] = conf;
    }
  }
  return frame;
}

/** The eight corners of a box, in world space. */
export function boxCorners(b: SynthBox): Vec3[] {
  const yaw = b.yawDeg ?? 0;
  const base = b.base ?? 0;
  const out: Vec3[] = [];
  for (const sx of [-1, 1]) {
    for (const sz of [-1, 1]) {
      const [x, z] = yawed(b.at, yaw, (sx * b.size[0]) / 2, (sz * b.size[2]) / 2);
      out.push([x, base, z], [x, base + b.size[1], z]);
    }
  }
  return out;
}

/**
 * Where world points appear in the UPRIGHT photo: the bounding rectangle of
 * their projections, as portrait fractions, grown by `pad` of its size per
 * side. What a vision model's region would be, if it were exactly right.
 */
export function regionOf(frame: DepthFrame, points: Vec3[], pad = 0): Region {
  const k = depthIntrinsics(frame);
  let x0 = Infinity;
  let y0 = Infinity;
  let x1 = -Infinity;
  let y1 = -Infinity;
  for (const w of points) {
    const uv = project(k, worldToCamera(frame, w));
    if (!uv) continue;
    x0 = Math.min(x0, uv[0] / frame.depthW);
    x1 = Math.max(x1, uv[0] / frame.depthW);
    y0 = Math.min(y0, uv[1] / frame.depthH);
    y1 = Math.max(y1, uv[1] / frame.depthH);
  }
  const sensor: Region = { x: x0, y: y0, w: x1 - x0, h: y1 - y0 };
  const p = sensorToPortrait(sensor, frame.orientation);
  return { x: p.x - p.w * pad, y: p.y - p.h * pad, w: p.w * (1 + 2 * pad), h: p.h * (1 + 2 * pad) };
}

/** A container's rim corners, for the guide rectangle a user would line up. */
export function rimCorners(c: SynthContainer): Vec3[] {
  const yaw = c.yawDeg ?? 0;
  const out: Vec3[] = [];
  for (const sx of [-1, 1]) {
    for (const sz of [-1, 1]) {
      const [x, z] = yawed(c.at, yaw, (sx * c.outer[0]) / 2, (sz * c.outer[2]) / 2);
      out.push([x, c.outer[1], z]);
    }
  }
  return out;
}
