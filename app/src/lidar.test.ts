import { attachLidarDimensions, measureFillFromFile } from "./lidar";
import { encodeDepth } from "./measure/depthfile";
import { lookAt, renderDepth } from "../test/synthdepth";
import type { ItemDraft } from "./types";

const item = (over: Partial<ItemDraft> = {}): ItemDraft => ({
  name: "box",
  category: "other",
  sizeBucket: "M",
  fragile: false,
  bulky: false,
  weightClass: "medium",
  notes: "",
  confidence: 0.9,
  quantity: 1,
  ...over,
});

// A 30 x 20 x 10 cm box on the floor, from 0.8 m at 45 degrees.
const boxFrame = encodeDepth(
  renderDepth({ boxes: [{ at: [0, 0], size: [0.3, 0.1, 0.2] }] }, lookAt([0, 0.05, 0], 0.8, 45, 0), { seed: 3 }),
);
const reads = (buf: ArrayBuffer) => async () => buf;

describe("sizes from a depth frame", () => {
  test("one item is measured, and marked as a measurement", async () => {
    const [got] = await attachLidarDimensions([item({ dimensionsCm: { l: 50, w: 40, h: 30 }, dimensionsSource: "vision" })], "x.depth", reads(boxFrame));
    expect(got?.dimensionsSource).toBe("lidar");
    expect(Math.abs((got?.dimensionsCm?.l ?? 0) - 30)).toBeLessThanOrEqual(1.5);
    expect(Math.abs((got?.dimensionsCm?.w ?? 0) - 20)).toBeLessThanOrEqual(1.5);
    expect(Math.abs((got?.dimensionsCm?.h ?? 0) - 10)).toBeLessThanOrEqual(1.5);
  });

  test("among several, an item the model did not locate keeps its estimate", async () => {
    const estimate = { dimensionsCm: { l: 12, w: 8, h: 5 }, dimensionsSource: "vision" as const };
    const got = await attachLidarDimensions([item(estimate), item({ ...estimate, name: "other" })], "x.depth", reads(boxFrame));
    expect(got.map((i) => i.dimensionsSource)).toEqual(["vision", "vision"]);
  });

  test("bulky things are not measured", async () => {
    const [got] = await attachLidarDimensions([item({ bulky: true })], "x.depth", reads(boxFrame));
    expect(got?.dimensionsCm).toBeUndefined();
  });

  test("an unreadable file changes nothing", async () => {
    const items = [item()];
    expect(await attachLidarDimensions(items, "x.depth", async () => new ArrayBuffer(8))).toBe(items);
    expect(await attachLidarDimensions(items, "x.depth", async () => Promise.reject(new Error("gone")))).toBe(items);
  });
});

describe("fill from a depth frame", () => {
  test("an unreadable file is a refusal with a way on, not a crash", async () => {
    const res = await measureFillFromFile("x.depth", { l: 60, w: 40, h: 35 }, async () => new ArrayBuffer(8));
    expect("refused" in res).toBe(true);
  });
});
