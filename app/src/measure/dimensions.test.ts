/**
 * Item sizes, graded against synthetic scenes whose true sizes are known.
 *
 * The tolerance is the plan's: 1.5 cm or 10%, whichever is larger. On these
 * frames (noise sigma 5 mm + 0.5% of depth, flying pixels, 2% holes) every
 * dimension landed within 1 cm from 0.6 to 1.3 m over four seeds, and within
 * 1.5 cm at 2 m; the slack is for the real sensor.
 */
import { boxCorners, lookAt, regionOf, renderDepth } from "../../test/synthdepth";
import type { SynthBox, SynthCamera, SynthOptions, SynthScene } from "../../test/synthdepth";
import { decodeDepth, encodeDepth } from "./depthfile";
import { measureItem } from "./dimensions";
import type { MeasuredSize } from "./dimensions";
import type { DepthFrame } from "./frame";

const BOX: SynthBox = { at: [0, 0], size: [0.3, 0.1, 0.2] };

/** Looking at the middle of `b` from `distance`, `pitch` below the horizon. */
function view(b: SynthBox, distance: number, pitch: number, yaw = 0): SynthCamera {
  return lookAt([b.at[0], (b.base ?? 0) + b.size[1] / 2, b.at[1]], distance, pitch, yaw);
}

// Rendering is most of this file's run time, and several tests look at the
// same scene, so frames are kept by what made them.
const frames = new Map<string, DepthFrame>();
function render(scene: SynthScene, cam: SynthCamera, opts: SynthOptions = {}): DepthFrame {
  const key = JSON.stringify([scene, cam, opts]);
  let f = frames.get(key);
  if (!f) {
    f = renderDepth(scene, cam, { seed: 11, ...opts });
    frames.set(key, f);
  }
  return f;
}

/** l >= w >= h in cm, each within max(1.5 cm, 10%). */
function expectSize(m: MeasuredSize | null, cm: [number, number, number]): void {
  expect(m).not.toBeNull();
  const got = [m?.l, m?.w, m?.h] as number[];
  const want = [...cm].sort((a, b) => b - a);
  got.forEach((g, i) => {
    const w = want[i] as number;
    expect(Math.abs(g - w)).toBeLessThanOrEqual(Math.max(1.5, w * 0.1));
  });
}

describe("measuring one item", () => {
  test("a 30x20x10 cm box at 0.8 m, 45 degrees down", () => {
    const f = render({ boxes: [BOX] }, view(BOX, 0.8, 45));
    const m = measureItem(f, regionOf(f, boxCorners(BOX), 0.1));
    expectSize(m, [30, 20, 10]);
    expect(m?.confidence).toBeGreaterThan(0.9);
  });

  test("the same box turned 30 degrees, seen from 30 degrees round", () => {
    const b = { ...BOX, yawDeg: 30 };
    const f = render({ boxes: [b] }, view(b, 0.8, 45, 30), { seed: 12 });
    expectSize(measureItem(f, regionOf(f, boxCorners(b), 0.1)), [30, 20, 10]);
  });

  test("a square footprint is not measured along its diagonal", () => {
    // PCA alone picks an arbitrary axis for a square, and extents along the
    // diagonal read up to 41% large.
    const b: SynthBox = { at: [0, 0], size: [0.2, 0.2, 0.2], yawDeg: 20 };
    const f = render({ boxes: [b] }, view(b, 0.8, 45), { seed: 13 });
    expectSize(measureItem(f, regionOf(f, boxCorners(b), 0.1)), [20, 20, 20]);
  });

  test("the survived trip through the file measures the same", () => {
    const f = render({ boxes: [BOX] }, view(BOX, 0.8, 45));
    const r = regionOf(f, boxCorners(BOX), 0.1);
    expect(measureItem(decodeDepth(encodeDepth(f)), r)).toEqual(measureItem(f, r));
  });
});

describe("which object", () => {
  // A second, smaller box beside the first, both inside one loose region: the
  // region is about the bigger thing in it.
  test("two boxes in one loose region: the larger one", () => {
    const small: SynthBox = { at: [0.26, 0.04], size: [0.1, 0.06, 0.08] };
    const f = render({ boxes: [BOX, small] }, lookAt([0.06, 0.05, 0], 0.8, 45, 0), { seed: 14 });
    const loose = regionOf(f, [...boxCorners(BOX), ...boxCorners(small)], 0.15);
    expectSize(measureItem(f, loose), [30, 20, 10]);
  });

  test("the smaller one, when the region is about it", () => {
    const small: SynthBox = { at: [0.26, 0.04], size: [0.1, 0.06, 0.08] };
    const f = render({ boxes: [BOX, small] }, lookAt([0.06, 0.05, 0], 0.8, 45, 0), { seed: 14 });
    expectSize(measureItem(f, regionOf(f, boxCorners(small), 0.1)), [10, 8, 6]);
  });

  test("no region: the object nearest the middle of the frame", () => {
    // A tall box off to the side is bigger, and is not what the camera is on.
    const side: SynthBox = { at: [-0.45, -0.1], size: [0.15, 0.3, 0.15] };
    const f = render({ boxes: [BOX, side] }, view(BOX, 0.8, 45), { seed: 15 });
    expectSize(measureItem(f), [30, 20, 10]);
  });
});

describe("when there is not enough to go on", () => {
  test("a 2 cm item is too few points to measure", () => {
    const tiny: SynthBox = { at: [0, 0], size: [0.02, 0.02, 0.02] };
    const f = render({ boxes: [tiny] }, view(tiny, 0.8, 45));
    expect(measureItem(f, regionOf(f, boxCorners(tiny), 0.3))).toBeNull();
    expect(measureItem(f)).toBeNull();
  });

  test("a bare floor has nothing standing on it", () => {
    const f = render({}, lookAt([0, 0, 0], 0.8, 45, 0));
    expect(measureItem(f)).toBeNull();
    expect(measureItem(f, { x: 0.3, y: 0.3, w: 0.4, h: 0.4 })).toBeNull();
  });

  test("distance costs confidence: full at 0.8 m, reduced at 2 m", () => {
    const near = render({ boxes: [BOX] }, view(BOX, 0.8, 45));
    const far = render({ boxes: [BOX] }, view(BOX, 2, 45));
    const a = measureItem(near, regionOf(near, boxCorners(BOX), 0.1));
    const b = measureItem(far, regionOf(far, boxCorners(BOX), 0.1));
    expect(b?.confidence).toBeLessThan(0.75);
    expect(b?.confidence).toBeLessThan(a?.confidence as number);
  });

  test("a floor the LiDAR cannot see falls back to ARKit's plane, trusted less", () => {
    const f = render({ boxes: [BOX], ground: false }, view(BOX, 0.8, 45));
    const m = measureItem(f, regionOf(f, boxCorners(BOX), 0.1));
    expectSize(m, [30, 20, 10]);
    expect(m?.confidence).toBeLessThan(0.9);
  });

  // With neither a visible floor nor an anchor, the lowest points in view are
  // assumed to be the floor -- a guess, and marked as one: below the 0.5 at
  // which the vision estimate is kept instead.
  test("with no plane at all it guesses, and says so", () => {
    const f = render({ boxes: [BOX], ground: false }, view(BOX, 0.8, 45), { groundAnchor: false });
    const m = measureItem(f, regionOf(f, boxCorners(BOX), 0.1));
    expect(m).not.toBeNull();
    expect(m?.confidence).toBeLessThan(0.5);
  });

  test("an unsupported orientation measures nothing rather than the wrong thing", () => {
    const f = render({ boxes: [BOX] }, view(BOX, 0.8, 45));
    expect(measureItem({ ...f, orientation: 5 })).toBeNull();
  });
});
