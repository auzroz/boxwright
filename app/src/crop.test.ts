import { coverCrop, cropRect } from "./crop";
import type { Region } from "./types";

const region = (x: number, y: number, w: number, h: number): Region => ({ x, y, w, h });

describe("cropping one item out of a shared photo", () => {
  test("a fractional box becomes a pixel rectangle in the source image", () => {
    // 1000x800, a box over the middle. Padding is 12% of the box's own size,
    // so 0.4 wide grows by 0.048 on each side.
    expect(cropRect(region(0.3, 0.25, 0.4, 0.5), 1000, 800)).toEqual({
      x: Math.round((0.3 - 0.048) * 1000),
      y: Math.round((0.25 - 0.06) * 800),
      width: Math.round((0.4 + 0.096) * 1000),
      height: Math.round((0.5 + 0.12) * 800),
    });
  });

  test("padding is clamped at the edges rather than running off the image", () => {
    const crop = cropRect(region(0, 0, 0.5, 0.5), 1000, 800)!;
    expect(crop.x).toBe(0);
    expect(crop.y).toBe(0);
    expect(crop.x + crop.width).toBeLessThanOrEqual(1000);
    expect(crop.y + crop.height).toBeLessThanOrEqual(800);
  });

  test("a box in the far corner stays inside the image", () => {
    const crop = cropRect(region(0.9, 0.9, 0.1, 0.1), 1000, 800)!;
    expect(crop.x).toBeGreaterThanOrEqual(0);
    expect(crop.y).toBeGreaterThanOrEqual(0);
    expect(crop.x + crop.width).toBeLessThanOrEqual(1000);
    expect(crop.y + crop.height).toBeLessThanOrEqual(800);
    expect(crop.width).toBeGreaterThan(0);
    expect(crop.height).toBeGreaterThan(0);
  });

  test("the aspect ratio of the source is respected, not assumed square", () => {
    // A square FRACTION over a tall image is a tall rectangle in pixels.
    const crop = cropRect(region(0.5, 0.5, 0.25, 0.25), 400, 1600)!;
    expect(crop.height).toBeGreaterThan(crop.width);
  });
});

describe("when there is nothing better to show than the whole photo", () => {
  test("no region at all", () => {
    expect(cropRect(undefined, 1000, 800)).toBeNull();
  });

  test("a region covering the whole frame", () => {
    expect(cropRect(region(0, 0, 1, 1), 1000, 800)).toBeNull();
  });

  test("a photo that has not been measured, or measures as zero", () => {
    expect(cropRect(region(0.1, 0.1, 0.5, 0.5), 0, 800)).toBeNull();
    expect(cropRect(region(0.1, 0.1, 0.5, 0.5), 1000, Number.NaN)).toBeNull();
  });

  test("nothing here ever throws, whatever a model sent", () => {
    for (const r of [region(-1, -1, 0, 0), region(2, 2, 1, 1), region(0.5, 0.5, 0, 0), region(Number.NaN, 0, 1, 1)]) {
      expect(() => cropRect(r, 1000, 800)).not.toThrow();
    }
  });
});

describe("drawing a crop into a square thumbnail", () => {
  test("the crop fills the box, and the box's corner is the crop's corner when it is square", () => {
    const crop = { x: 100, y: 200, width: 400, height: 400 };
    const at = coverCrop(crop, { width: 1000, height: 800 }, 56);
    const scale = 56 / 400;
    expect(at.width).toBeCloseTo(1000 * scale);
    expect(at.height).toBeCloseTo(800 * scale);
    expect(at.left).toBeCloseTo(-100 * scale);
    expect(at.top).toBeCloseTo(-200 * scale);
  });

  test("a wide crop is scaled to the box's height and centred across it", () => {
    const crop = { x: 0, y: 0, width: 800, height: 400 };
    const at = coverCrop(crop, { width: 1000, height: 800 }, 56);
    const scale = 56 / 400;
    // 800 * scale = 112 wide in a 56 box: 28 hidden on each side.
    expect(at.left).toBeCloseTo(-28);
    expect(at.top).toBeCloseTo(0);
    expect(at.height).toBeCloseTo(800 * scale);
  });
});
