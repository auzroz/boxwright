/**
 * Fill levels, graded against synthetic containers of known contents.
 *
 * The container: 60 x 40 cm outside, 35 cm tall, 1.2 cm walls (about a
 * plastic container's rim lip) and a 1 cm floor, so 57.6 x 37.6 x 34 cm
 * inside -- turned 15 degrees,
 * looked into from 0.9 m above its rim. The plan's tolerance is 10 points; on
 * these frames every level currently reads within 3.
 */
import { lookAt, regionOf, renderDepth, rimCorners } from "../../test/synthdepth";
import type { SynthContainer } from "../../test/synthdepth";
import { measureFill } from "./fill";
import type { FillResult } from "./fill";
import type { DepthFrame } from "./frame";

const OUTER_H = 0.35;
const FLOOR = 0.01;
const DEPTH = OUTER_H - FLOOR;
const INTERIOR = { l: 57.6, w: 37.6, h: DEPTH * 100 };

function container(contents: SynthContainer["contents"]): SynthContainer {
  return { at: [0, 0], outer: [0.6, OUTER_H, 0.4], wall: 0.012, floor: FLOOR, yawDeg: 15, contents };
}

/** `tilt` degrees from straight down, facing 30 degrees round. */
function measure(c: SynthContainer, tilt: number, seed = 21): FillResult {
  const f = renderDepth({ containers: [c] }, lookAt([0, OUTER_H, 0], 0.9, 90 - tilt, 30), { seed });
  // The guide rectangle a user would line the rim up with.
  return measureFill(f, regionOf(f, rimCorners(c), 0), INTERIOR);
}

function level(r: FillResult): { fillPct: number; overfull: boolean; confidence: number } {
  if ("refused" in r) throw new Error(`refused: ${r.refused}`);
  return r;
}

describe("how full, at 20 degrees from straight down", () => {
  test("empty", () => {
    const r = level(measure(container([]), 20));
    expect(r.fillPct).toBeLessThanOrEqual(10);
    expect(r.overfull).toBe(false);
    expect(r.confidence).toBeGreaterThan(0.5);
  });

  test("half full", () => {
    const r = level(measure(container([{ top: DEPTH / 2, inset: 0 }]), 20));
    expect(Math.abs(r.fillPct - 50)).toBeLessThanOrEqual(10);
    expect(r.overfull).toBe(false);
  });

  // Level with the rim is full, and NOT overfull: the rim is only found to
  // about a centimetre, and a bare "mean > 1" called this overfull every time.
  test("full to the rim", () => {
    const r = level(measure(container([{ top: DEPTH, inset: 0 }]), 20));
    expect(r.fillPct).toBeGreaterThanOrEqual(90);
    expect(r.overfull).toBe(false);
  });

  // Heaped 25% past the rim over the middle. The heap looms larger on the near
  // side at a tilt and reaches into the band searched for the rim; the rim is
  // still found, because it is the height that goes all the way round.
  test("heaped past the rim is overfull", () => {
    const r = level(
      measure(
        container([
          { top: DEPTH, inset: 0 },
          { top: DEPTH * 1.25, inset: 0.2 },
        ]),
        20,
      ),
    );
    expect(r.fillPct).toBe(100);
    expect(r.overfull).toBe(true);
    expect(r.confidence).toBeGreaterThan(0.5);
  });
});

let floor: DepthFrame | null = null;
function bareFloor(): DepthFrame {
  floor ??= renderDepth({}, lookAt([0, 0, 0], 0.9, 90, 0), { seed: 1 });
  return floor;
}

describe("refusing", () => {
  test("40 degrees from straight down", () => {
    const r = measure(container([{ top: DEPTH / 2, inset: 0 }]), 40);
    expect("refused" in r && r.refused).toMatch(/tilted 40° from straight down/);
  });

  test("without the inside size", () => {
    // Checked before the frame is even looked at, so any frame will do.
    const r = measureFill(bareFloor(), { x: 0.2, y: 0.2, w: 0.6, h: 0.6 }, { l: 50, w: 30, h: 0 });
    expect("refused" in r && r.refused).toMatch(/inside size/);
  });

  test("a guide on bare floor finds no rim", () => {
    const r = measureFill(bareFloor(), { x: 0.2, y: 0.2, w: 0.6, h: 0.6 }, INTERIOR);
    expect("refused" in r).toBe(true);
  });
});
