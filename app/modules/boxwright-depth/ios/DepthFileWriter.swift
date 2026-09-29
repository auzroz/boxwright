//
//  DepthFileWriter.swift
//  BoxwrightDepth
//
//  Writes one LiDAR frame as a `.depth` file: the BWD1 layout that
//  app/src/measure/depthfile.ts reads. The two must agree byte for byte, so
//  the layout is repeated here rather than referred to:
//
//    offset  size        field
//    0       4           magic "BWD1"
//    4       u16         version (1)
//    6       u16 x2      depthW, depthH   -- sensor orientation (256x192)
//    10      u16 x2      imageW, imageH   -- capturedImage (1920x1440)
//    14      f32 x9      intrinsics, ROW-major, in capturedImage pixels
//    50      f32 x16     camera-to-world, COLUMN-major (simd_float4x4 memory order)
//    114     u8          EXIF orientation of the upright photo (6 = .right)
//    115     u8          plane count P
//    116     P x 72      per plane: f32 x16 anchor-to-world (column-major,
//                        centre and y-rotation baked in), f32 x2 extent (x, z)
//    ...     u16 x W*H   depth in millimetres, row-major; 0 = no reading
//    ...     u8  x W*H   ARConfidenceLevel, 0 / 1 / 2
//
//  Everything little-endian, which is what every iPhone is natively.
//

import ARKit
import Foundation

/// The parts of an ARFrame a `.depth` file needs, copied out of it.
///
/// ARKit recycles a small pool of frames; holding one while a JPEG encodes
/// starves the session and it starts dropping frames. So the frame is read
/// once, into plain values, and let go.
struct DepthSnapshot {
  let depthW: Int
  let depthH: Int
  let imageW: Int
  let imageH: Int
  /// Row-major, 9 values.
  let intrinsics: [Float]
  /// Column-major, 16 values.
  let cameraTransform: [Float]
  let planes: [(transform: [Float], extent: (Float, Float))]
  /// Millimetres, row-major, 0 where there was no reading.
  let depthMm: [UInt16]
  /// Row-major; all 1 (medium) when ARKit gave no confidence map.
  let confidence: [UInt8]

  /// EXIF orientation 6: the upright portrait photo is the sensor image turned
  /// 90 degrees clockwise. The app is portrait-only, so this never changes.
  static let orientationRight: UInt8 = 6

  /// Copies what is needed out of `frame`, or nil when it carries no depth.
  static func copy(from frame: ARFrame) -> DepthSnapshot? {
    guard let depth = frame.smoothedSceneDepth ?? frame.sceneDepth else { return nil }
    let map = depth.depthMap
    let w = CVPixelBufferGetWidth(map)
    let h = CVPixelBufferGetHeight(map)
    guard CVPixelBufferGetPixelFormatType(map) == kCVPixelFormatType_DepthFloat32 else { return nil }

    var mm = [UInt16](repeating: 0, count: w * h)
    CVPixelBufferLockBaseAddress(map, .readOnly)
    if let base = CVPixelBufferGetBaseAddress(map) {
      let stride = CVPixelBufferGetBytesPerRow(map)
      for y in 0..<h {
        let row = (base + y * stride).assumingMemoryBound(to: Float32.self)
        for x in 0..<w {
          let metres = row[x]
          // NaN and anything non-positive are "no reading"; past 65.535 m
          // is beyond any LiDAR and clamps.
          if metres.isFinite && metres > 0 {
            mm[y * w + x] = UInt16(min(65535, (metres * 1000).rounded()))
          }
        }
      }
    }
    CVPixelBufferUnlockBaseAddress(map, .readOnly)

    var conf = [UInt8](repeating: 1, count: w * h)
    if let cmap = depth.confidenceMap,
      CVPixelBufferGetWidth(cmap) == w, CVPixelBufferGetHeight(cmap) == h
    {
      CVPixelBufferLockBaseAddress(cmap, .readOnly)
      if let base = CVPixelBufferGetBaseAddress(cmap) {
        let stride = CVPixelBufferGetBytesPerRow(cmap)
        for y in 0..<h {
          let row = (base + y * stride).assumingMemoryBound(to: UInt8.self)
          for x in 0..<w { conf[y * w + x] = row[x] }
        }
      }
      CVPixelBufferUnlockBaseAddress(cmap, .readOnly)
    }

    // simd matrices are column-major: m[c][r] is row r of column c. The file
    // wants the intrinsics ROW-major, so they are written transposed.
    let k = frame.camera.intrinsics
    var intrinsics: [Float] = []
    for r in 0..<3 {
      for c in 0..<3 { intrinsics.append(k[c][r]) }
    }

    return DepthSnapshot(
      depthW: w,
      depthH: h,
      imageW: CVPixelBufferGetWidth(frame.capturedImage),
      imageH: CVPixelBufferGetHeight(frame.capturedImage),
      intrinsics: intrinsics,
      cameraTransform: columnMajor(frame.camera.transform),
      planes: frame.anchors.compactMap { $0 as? ARPlaneAnchor }.prefix(255).map(planeRecord),
      depthMm: mm,
      confidence: conf
    )
  }

  /// A plane as the file stores it: the anchor's transform with the plane's
  /// own centre (and, from iOS 16, its rotation about y) baked in, so the
  /// extent is centred on the transform's origin and runs along its x and z.
  private static func planeRecord(_ anchor: ARPlaneAnchor) -> (transform: [Float], extent: (Float, Float)) {
    var local = matrix_identity_float4x4
    local.columns.3 = SIMD4<Float>(anchor.center, 1)
    var extent = (anchor.extent.x, anchor.extent.z)
    if #available(iOS 16.0, *) {
      let e = anchor.planeExtent
      let a = e.rotationOnYAxis
      // Rotation about y by `a`, as columns.
      let rot = simd_float4x4(
        SIMD4<Float>(cos(a), 0, -sin(a), 0),
        SIMD4<Float>(0, 1, 0, 0),
        SIMD4<Float>(sin(a), 0, cos(a), 0),
        SIMD4<Float>(0, 0, 0, 1)
      )
      local = local * rot
      extent = (e.width, e.height)
    }
    return (columnMajor(anchor.transform * local), extent)
  }

  private static func columnMajor(_ m: simd_float4x4) -> [Float] {
    [m.columns.0, m.columns.1, m.columns.2, m.columns.3].flatMap { [$0.x, $0.y, $0.z, $0.w] }
  }

  /// The encoded file.
  func encoded() -> Data {
    var d = Data(capacity: 116 + planes.count * 72 + depthW * depthH * 3)
    d.append(contentsOf: Array("BWD1".utf8))
    append(&d, UInt16(1))
    append(&d, UInt16(depthW))
    append(&d, UInt16(depthH))
    append(&d, UInt16(imageW))
    append(&d, UInt16(imageH))
    for v in intrinsics { append(&d, v) }
    for v in cameraTransform { append(&d, v) }
    d.append(DepthSnapshot.orientationRight)
    d.append(UInt8(planes.count))
    for p in planes {
      for v in p.transform { append(&d, v) }
      append(&d, p.extent.0)
      append(&d, p.extent.1)
    }
    depthMm.withUnsafeBufferPointer { buf in
      // Little-endian in memory on every iPhone, so the array's bytes ARE the
      // file's bytes.
      d.append(UnsafeBufferPointer(start: UnsafeRawPointer(buf.baseAddress!).assumingMemoryBound(to: UInt8.self),
                                   count: buf.count * 2))
    }
    d.append(contentsOf: confidence)
    return d
  }

  private func append(_ d: inout Data, _ v: UInt16) {
    withUnsafeBytes(of: v.littleEndian) { d.append(contentsOf: $0) }
  }

  private func append(_ d: inout Data, _ v: Float) {
    withUnsafeBytes(of: v.bitPattern.littleEndian) { d.append(contentsOf: $0) }
  }
}
