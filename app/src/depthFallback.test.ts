import type { DepthStatus } from "boxwright-depth";

import { POOR_TRACKING_MS, START_TIMEOUT_MS, cameraFor, depthScreen } from "./depthFallback";

const ok: DepthStatus = { tracking: "normal", tiltFromDownDeg: 10, depthOK: true, centerDistanceM: 0.8 };
const base = { mode: "item" as const, status: null, event: null, openedAt: 0, now: 0, poorSince: null };

describe("which camera", () => {
  test("no LiDAR: the system camera, as always", () => {
    expect(cameraFor({ isSupported: false, cameraAccess: "granted" })).toBe("system");
  });
  test("LiDAR and access (or not yet asked): ours", () => {
    expect(cameraFor({ isSupported: true, cameraAccess: "granted" })).toBe("depth");
    expect(cameraFor({ isSupported: true, cameraAccess: "undetermined" })).toBe("depth");
  });
  // ARKit refused the camera is a black screen; the system camera says why and offers Settings.
  test("access refused: the system camera, which explains itself", () => {
    expect(cameraFor({ isSupported: true, cameraAccess: "denied" })).toBe("system");
    expect(cameraFor({ isSupported: true, cameraAccess: "restricted" })).toBe("system");
  });
});

describe("the camera screen", () => {
  test("starting, then not starting: the way round appears after the timeout", () => {
    expect(depthScreen({ ...base, now: 1000 })).toMatchObject({ hint: "Starting the camera…", offerFallback: false, canShoot: false });
    expect(depthScreen({ ...base, now: START_TIMEOUT_MS })).toMatchObject({ hint: "The camera hasn’t started.", offerFallback: true });
  });

  test("a failure replaces the shutter, and names Settings when access is the cause", () => {
    const denied = depthScreen({ ...base, event: { state: "failed", cameraDenied: true } });
    expect(denied).toMatchObject({ failed: true, openSettings: true, offerFallback: true, canShoot: false });
    const other = depthScreen({ ...base, event: { state: "failed", message: "Camera in use", cameraDenied: false } });
    expect(other.problem).toBe("The camera stopped: Camera in use");
    expect(other.openSettings).toBe(false);
  });

  test("an interruption waits, with the way round offered", () => {
    expect(depthScreen({ ...base, status: ok, event: { state: "interrupted", cameraDenied: false } })).toMatchObject({
      failed: false,
      canShoot: false,
      offerFallback: true,
    });
    // Resumed is back to normal.
    expect(depthScreen({ ...base, status: ok, event: { state: "resumed", cameraDenied: false } }).canShoot).toBe(true);
  });

  test("poor tracking: a photo still works; after a while, a suggestion", () => {
    const limited: DepthStatus = { ...ok, tracking: "limited" };
    const early = depthScreen({ ...base, status: limited, now: 1000, poorSince: 0 });
    expect(early).toMatchObject({ canShoot: true, offerFallback: false });
    const late = depthScreen({ ...base, status: limited, now: POOR_TRACKING_MS, poorSince: 0 });
    expect(late.hint).toMatch(/More light/);
    expect(late.offerFallback).toBe(true);
  });

  test("fill needs tracking to measure, and can always be answered by hand", () => {
    const limited: DepthStatus = { ...ok, tracking: "limited" };
    expect(depthScreen({ ...base, mode: "fill", status: limited, now: 1, poorSince: 0 }).canShoot).toBe(false);
    const good = depthScreen({ ...base, mode: "fill", status: ok });
    expect(good).toMatchObject({ canShoot: true, offerFallback: true });
    expect(depthScreen({ ...base, mode: "fill", status: { ...ok, tiltFromDownDeg: 42 } }).hint).toMatch(/Tilted 42°/);
  });

  test("ready, or no depth at this distance -- the photo works either way", () => {
    expect(depthScreen({ ...base, status: ok }).hint).toBe("Ready.");
    expect(depthScreen({ ...base, status: { ...ok, depthOK: false } })).toMatchObject({ canShoot: true, hint: expect.stringMatching(/still works/) });
  });
});
