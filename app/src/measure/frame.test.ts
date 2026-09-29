/**
 * The conventions. Every measurement is only as right as the mapping from the
 * photo's region to the depth pixels and the direction of "up", and a device
 * is the only thing that can confirm them -- so they are pinned here
 * explicitly, in the form the native side has to match.
 */
import { cameraTransform, lookAt, renderDepth, regionOf } from "../../test/synthdepth";
import type { Region } from "../types";
import {
  ORIENTATION_DOWN,
  ORIENTATION_LEFT,
  ORIENTATION_RIGHT,
  ORIENTATION_UP,
  backProject,
  cameraToWorld,
  depthIntrinsics,
  portraitToSensor,
  project,
  sensorToPortrait,
  upVector,
  worldToCamera,
} from "./frame";
import type { DepthFrame, Vec3 } from "./frame";

function expectRegion(actual: Region, expected: Region): void {
  expect(actual.x).toBeCloseTo(expected.x, 9);
  expect(actual.y).toBeCloseTo(expected.y, 9);
  expect(actual.w).toBeCloseTo(expected.w, 9);
  expect(actual.h).toBeCloseTo(expected.h, 9);
}

function expectVec(actual: Vec3, expected: Vec3, digits = 6): void {
  actual.forEach((v, i) => expect(v).toBeCloseTo(expected[i] as number, digits));
}

/** A frame with no depth, for the geometry alone. */
function poseOnly(pitchDeg: number, yawDeg: number): DepthFrame {
  const cam = lookAt([0, 1, 0], 1, pitchDeg, yawDeg);
  return {
    depthW: 256,
    depthH: 192,
    imageW: 1920,
    imageH: 1440,
    intrinsics: [1450, 0, 960, 0, 1450, 720, 0, 0, 1],
    cameraTransform: cameraTransform(cam),
    orientation: ORIENTATION_RIGHT,
    planes: [],
    depthMm: new Uint16Array(256 * 192),
    confidence: new Uint8Array(256 * 192),
  };
}

describe("a region of the upright photo, on the sensor", () => {
  // `.right`: the photo is the sensor image turned 90 degrees clockwise. The
  // photo's top-left is the sensor's bottom-left; its top edge is the
  // sensor's left edge.
  test("portrait `.right` maps x, y, w, h as xs = y, ys = 1 - x - w", () => {
    const r = { x: 0.1, y: 0.2, w: 0.3, h: 0.4 };
    expectRegion(portraitToSensor(r, ORIENTATION_RIGHT), { x: 0.2, y: 0.6, w: 0.4, h: 0.3 });
    // A sliver along the photo's top edge is a sliver along the sensor's left.
    expectRegion(portraitToSensor({ x: 0, y: 0, w: 1, h: 0.1 }, ORIENTATION_RIGHT), { x: 0, y: 0, w: 0.1, h: 1 });
    // The photo's right edge is the sensor's top.
    expectRegion(portraitToSensor({ x: 0.9, y: 0, w: 0.1, h: 1 }, ORIENTATION_RIGHT), { x: 0, y: 0, w: 1, h: 0.1 });
  });

  test("every orientation round-trips, and the whole frame is the whole frame", () => {
    const r = { x: 0.12, y: 0.34, w: 0.21, h: 0.05 };
    for (const o of [ORIENTATION_UP, ORIENTATION_DOWN, ORIENTATION_RIGHT, ORIENTATION_LEFT]) {
      expectRegion(sensorToPortrait(portraitToSensor(r, o), o), r);
      expectRegion(portraitToSensor({ x: 0, y: 0, w: 1, h: 1 }, o), { x: 0, y: 0, w: 1, h: 1 });
    }
    // `.left` is `.right` the other way round: the photo's top edge is the
    // sensor's RIGHT edge.
    expectRegion(portraitToSensor({ x: 0, y: 0, w: 1, h: 0.1 }, ORIENTATION_LEFT), { x: 0.9, y: 0, w: 0.1, h: 1 });
  });

  test("a mirrored or unknown orientation is refused, never guessed", () => {
    expect(() => portraitToSensor({ x: 0, y: 0, w: 1, h: 1 }, 2)).toThrow(/orientation/);
    expect(() => sensorToPortrait({ x: 0, y: 0, w: 1, h: 1 }, 0)).toThrow(/orientation/);
  });
});

describe("ARKit's axes, for a phone held upright", () => {
  // ARKit's camera x runs along the sensor's long side, which in portrait
  // points at the floor. Getting this wrong measures everything sideways.
  test("up, seen from a level portrait camera, is -x in camera space", () => {
    expectVec(upVector(poseOnly(0, 0)), [-1, 0, 0]);
    expectVec(upVector(poseOnly(0, 70)), [-1, 0, 0]);
  });

  test("looking straight down, up is straight back along the view", () => {
    expectVec(upVector(poseOnly(90, 0)), [0, 0, -1]);
  });

  test("something higher in the world is higher in the upright photo", () => {
    const frame = poseOnly(20, 35);
    const aim = cameraToWorld(frame, [0, 0, 1]);
    const above = regionOf(frame, [[aim[0], aim[1] + 0.2, aim[2]]]);
    const below = regionOf(frame, [[aim[0], aim[1] - 0.2, aim[2]]]);
    expect(above.y).toBeLessThan(0.5);
    expect(below.y).toBeGreaterThan(0.5);
    // And sideways stays sideways: level with the aim point, it is mid-height.
    expect(regionOf(frame, [aim]).y).toBeCloseTo(0.5, 3);
  });

  test("something to the right in the world is right in the upright photo", () => {
    // Facing -z with no yaw, the world's +x is to the right.
    const frame = poseOnly(10, 0);
    const aim = cameraToWorld(frame, [0, 0, 1]);
    expect(regionOf(frame, [[aim[0] + 0.2, aim[1], aim[2]]]).x).toBeGreaterThan(0.5);
    expect(regionOf(frame, [[aim[0] - 0.2, aim[1], aim[2]]]).x).toBeLessThan(0.5);
  });

  test("camera and world round-trip", () => {
    const frame = poseOnly(33, -140);
    const p: Vec3 = [0.1, -0.2, 0.9];
    expectVec(worldToCamera(frame, cameraToWorld(frame, p)), p);
  });
});

describe("back-projection", () => {
  test("intrinsics scale from the image to the depth map", () => {
    const k = depthIntrinsics(poseOnly(0, 0));
    expect(k.fx).toBeCloseTo(1450 * (256 / 1920), 6);
    expect(k.cx).toBeCloseTo(128, 6);
    expect(k.cy).toBeCloseTo(96, 6);
  });

  test("a pixel back-projected and projected again is the same pixel", () => {
    const k = depthIntrinsics(poseOnly(0, 0));
    const p = backProject(k, 40.5, 150.5, 1.25);
    expect(p[2]).toBe(1.25);
    const uv = project(k, p);
    expect(uv?.[0]).toBeCloseTo(40.5, 6);
    expect(uv?.[1]).toBeCloseTo(150.5, 6);
  });

  test("the floor under a synthetic camera is where the pose says", () => {
    // 1 m above the floor, looking 90 degrees down: the middle pixel is 1 m away.
    const frame = renderDepth({}, lookAt([0, 0, 0], 1, 90, 0), { noise: 0, holes: 0 });
    const mid = 96 * 256 + 128;
    expect(frame.depthMm[mid]).toBe(1000);
    // And every floor point is 1 m below the camera, whichever pixel it is.
    const k = depthIntrinsics(frame);
    const up = upVector(frame);
    for (const [i, j] of [
      [3, 5],
      [250, 180],
      [128, 10],
    ] as const) {
      const p = backProject(k, i + 0.5, j + 0.5, (frame.depthMm[j * 256 + i] as number) / 1000);
      expect(up[0] * p[0] + up[1] * p[1] + up[2] * p[2]).toBeCloseTo(-1, 2);
    }
  });
});
