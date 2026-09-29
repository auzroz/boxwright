/**
 * The `.depth` file is written by Swift and read here, possibly hours apart
 * with the app killed in between, so it is checked byte-for-byte: a frame
 * survives the trip, and anything that is not a whole version-1 file is
 * refused rather than measured.
 */
import { lookAt, renderDepth } from "../../test/synthdepth";
import { DEPTH_FILE_VERSION, decodeDepth, depthFileSize, encodeDepth } from "./depthfile";
import type { DepthFrame } from "./frame";

let cached: DepthFrame | null = null;
function frame(): DepthFrame {
  cached ??= renderDepth({ boxes: [{ at: [0, 0], size: [0.3, 0.1, 0.2] }] }, lookAt([0, 0.05, 0], 0.8, 45, 20), {
    seed: 7,
  });
  return cached;
}

describe("the depth file", () => {
  test("round-trips a frame", () => {
    const f = frame();
    const back = decodeDepth(encodeDepth(f));
    expect(back.depthW).toBe(256);
    expect(back.depthH).toBe(192);
    expect(back.imageW).toBe(1920);
    expect(back.imageH).toBe(1440);
    expect(back.orientation).toBe(f.orientation);
    expect(back.depthMm).toEqual(f.depthMm);
    expect(back.confidence).toEqual(f.confidence);
    // f32 on disk: equal to float precision, not to the last double bit.
    back.intrinsics.forEach((v, i) => expect(v).toBeCloseTo(f.intrinsics[i] as number, 3));
    back.cameraTransform.forEach((v, i) => expect(v).toBeCloseTo(f.cameraTransform[i] as number, 6));
    expect(back.planes).toHaveLength(1);
    back.planes[0]?.transform.forEach((v, i) => expect(v).toBeCloseTo(f.planes[0]?.transform[i] as number, 6));
    expect(back.planes[0]?.extent).toEqual(f.planes[0]?.extent);
  });

  test("is about 145 KB for a 256x192 frame", () => {
    const bytes = encodeDepth(frame()).byteLength;
    expect(bytes).toBe(depthFileSize(256, 192, 1));
    expect(bytes).toBe(116 + 72 + 256 * 192 * 3);
    expect(bytes / 1024).toBeCloseTo(144, 0);
  });

  test("lays the header out as documented, little-endian", () => {
    const v = new DataView(encodeDepth(frame()));
    expect(String.fromCharCode(v.getUint8(0), v.getUint8(1), v.getUint8(2), v.getUint8(3))).toBe("BWD1");
    expect(v.getUint16(4, true)).toBe(DEPTH_FILE_VERSION);
    expect(v.getUint16(6, true)).toBe(256);
    expect(v.getUint16(8, true)).toBe(192);
    expect(v.getFloat32(14, true)).toBeCloseTo(1450, 3); // fx, row-major
    expect(v.getFloat32(14 + 2 * 4, true)).toBeCloseTo(960, 3); // cx
    expect(v.getFloat32(50 + 15 * 4, true)).toBe(1); // column-major: m[15]
    expect(v.getUint8(114)).toBe(6); // `.right`
    expect(v.getUint8(115)).toBe(1); // one plane
  });

  test("reads from a Uint8Array view into a larger buffer", () => {
    const enc = new Uint8Array(encodeDepth(frame()));
    const padded = new Uint8Array(enc.length + 16);
    padded.set(enc, 8);
    expect(decodeDepth(padded.subarray(8, 8 + enc.length)).depthMm).toEqual(frame().depthMm);
  });

  test("refuses a half-written file", () => {
    const enc = encodeDepth(frame());
    expect(() => decodeDepth(enc.slice(0, enc.byteLength - 1))).toThrow(/bytes/);
    expect(() => decodeDepth(enc.slice(0, 40))).toThrow(/too short/);
  });

  test("refuses anything that is not a version-1 depth file", () => {
    const bad = new Uint8Array(encodeDepth(frame()));
    bad[0] = 0x4a;
    expect(() => decodeDepth(bad)).toThrow(/magic/);

    const v2 = new Uint8Array(encodeDepth(frame()));
    new DataView(v2.buffer).setUint16(4, 2, true);
    expect(() => decodeDepth(v2)).toThrow(/version 2/);

    const mirrored = new Uint8Array(encodeDepth(frame()));
    mirrored[114] = 2;
    expect(() => decodeDepth(mirrored)).toThrow(/orientation/);
  });

  test("will not write arrays that do not match the dimensions", () => {
    expect(() => encodeDepth({ ...frame(), depthMm: new Uint16Array(10) })).toThrow(/want 49152/);
  });
});
