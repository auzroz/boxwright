// The `.depth` file: one LiDAR frame as the native camera writes it beside the
// photo, and as the measurement code reads it back.
//
// A file of our own rather than an image format, because what matters is not
// the depth alone but the geometry it was taken with -- the intrinsics, the
// camera's pose against gravity, the planes ARKit had found -- and all of it
// has to survive a capture being parked for hours. About 145 KB at 256x192.
// It never leaves the phone: only the numbers measured from it do.
//
// Layout, all LITTLE-endian, no padding:
//
//   offset  size        field
//   0       4           magic "BWD1"
//   4       u16         version (1)
//   6       u16 x2      depthW, depthH   -- sensor orientation, e.g. 256x192
//   10      u16 x2      imageW, imageH   -- capturedImage, e.g. 1920x1440
//   14      f32 x9      intrinsics, ROW-major [fx 0 cx; 0 fy cy; 0 0 1], in
//                       imageW x imageH pixels. (ARKit's simd_float3x3 is
//                       column-major: the native side writes it transposed.)
//   50      f32 x16     camera-to-world, COLUMN-major exactly as ARKit's
//                       simd_float4x4 sits in memory (translation at 12..14)
//   114     u8          EXIF orientation of the upright photo: 6 (`.right`)
//                       for a phone held in portrait
//   115     u8          plane count P
//   116     P x 72      per plane: f32 x16 anchor-to-world (column-major, centre
//                       baked in) then f32 x2 extent (x, z) in metres
//   ...     u16 x W*H   depth in millimetres, row-major; 0 = no reading
//   ...     u8  x W*H   ARKit confidence, 0 low / 1 medium / 2 high

import type { DepthFrame, DepthPlane } from "./frame";
import { isSupportedOrientation } from "./frame";

const MAGIC = [0x42, 0x57, 0x44, 0x31]; // "BWD1"
export const DEPTH_FILE_VERSION = 1;
const HEADER = 116;
const PLANE_BYTES = 72;

/** The exact size of a file for these dimensions, for callers that want to check one. */
export function depthFileSize(depthW: number, depthH: number, planes: number): number {
  return HEADER + planes * PLANE_BYTES + depthW * depthH * 3;
}

export function encodeDepth(frame: DepthFrame): ArrayBuffer {
  const n = frame.depthW * frame.depthH;
  if (frame.depthMm.length !== n || frame.confidence.length !== n) {
    throw new Error(`depth arrays hold ${frame.depthMm.length}/${frame.confidence.length} values, want ${n}`);
  }
  if (frame.intrinsics.length !== 9 || frame.cameraTransform.length !== 16) {
    throw new Error("intrinsics need 9 values and the camera transform 16");
  }
  if (frame.planes.length > 255) throw new Error(`${frame.planes.length} planes; at most 255 fit`);

  const buf = new ArrayBuffer(depthFileSize(frame.depthW, frame.depthH, frame.planes.length));
  const v = new DataView(buf);
  MAGIC.forEach((b, i) => v.setUint8(i, b));
  v.setUint16(4, DEPTH_FILE_VERSION, true);
  v.setUint16(6, frame.depthW, true);
  v.setUint16(8, frame.depthH, true);
  v.setUint16(10, frame.imageW, true);
  v.setUint16(12, frame.imageH, true);
  let o = 14;
  for (const x of frame.intrinsics) {
    v.setFloat32(o, x, true);
    o += 4;
  }
  for (const x of frame.cameraTransform) {
    v.setFloat32(o, x, true);
    o += 4;
  }
  v.setUint8(o++, frame.orientation);
  v.setUint8(o++, frame.planes.length);
  for (const p of frame.planes) {
    if (p.transform.length !== 16) throw new Error("a plane transform needs 16 values");
    for (const x of p.transform) {
      v.setFloat32(o, x, true);
      o += 4;
    }
    v.setFloat32(o, p.extent[0], true);
    v.setFloat32(o + 4, p.extent[1], true);
    o += 8;
  }
  for (let i = 0; i < n; i++) {
    v.setUint16(o, frame.depthMm[i] as number, true);
    o += 2;
  }
  new Uint8Array(buf, o, n).set(frame.confidence);
  return buf;
}

/**
 * Reads a `.depth` file. Throws, with the reason, on anything that is not
 * exactly a version-1 file: a half-written file from a capture the app was
 * killed during must never be measured as though it were whole.
 */
export function decodeDepth(data: ArrayBuffer | Uint8Array): DepthFrame {
  const bytes = data instanceof Uint8Array ? data : new Uint8Array(data);
  const v = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  if (bytes.byteLength < HEADER) throw new Error(`depth file is ${bytes.byteLength} bytes, too short for a header`);
  for (let i = 0; i < 4; i++) {
    if (v.getUint8(i) !== MAGIC[i]) throw new Error("not a depth file (bad magic)");
  }
  const version = v.getUint16(4, true);
  if (version !== DEPTH_FILE_VERSION) throw new Error(`unsupported depth file version ${version}`);

  const depthW = v.getUint16(6, true);
  const depthH = v.getUint16(8, true);
  const imageW = v.getUint16(10, true);
  const imageH = v.getUint16(12, true);
  if (!depthW || !depthH || !imageW || !imageH) throw new Error("depth file has a zero dimension");

  let o = 14;
  const intrinsics: number[] = [];
  for (let i = 0; i < 9; i++, o += 4) intrinsics.push(v.getFloat32(o, true));
  const cameraTransform: number[] = [];
  for (let i = 0; i < 16; i++, o += 4) cameraTransform.push(v.getFloat32(o, true));
  const orientation = v.getUint8(o++);
  if (!isSupportedOrientation(orientation)) throw new Error(`unsupported image orientation ${orientation}`);
  const planeCount = v.getUint8(o++);

  const want = depthFileSize(depthW, depthH, planeCount);
  if (bytes.byteLength !== want) {
    throw new Error(`depth file is ${bytes.byteLength} bytes; a ${depthW}x${depthH} frame with ${planeCount} planes is ${want}`);
  }

  const planes: DepthPlane[] = [];
  for (let p = 0; p < planeCount; p++) {
    const transform: number[] = [];
    for (let i = 0; i < 16; i++, o += 4) transform.push(v.getFloat32(o, true));
    planes.push({ transform, extent: [v.getFloat32(o, true), v.getFloat32(o + 4, true)] });
    o += 8;
  }

  const n = depthW * depthH;
  const depthMm = new Uint16Array(n);
  for (let i = 0; i < n; i++, o += 2) depthMm[i] = v.getUint16(o, true);
  const confidence = bytes.slice(o, o + n);

  return { depthW, depthH, imageW, imageH, intrinsics, cameraTransform, orientation, planes, depthMm, confidence };
}
