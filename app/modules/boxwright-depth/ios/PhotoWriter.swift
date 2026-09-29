//
//  PhotoWriter.swift
//  BoxwrightDepth
//
//  The capture's photo: ARKit's camera image, upright, as a JPEG with nothing
//  in it but pixels.
//

import CoreImage
import CoreVideo
import Foundation
import ImageIO
import UniformTypeIdentifiers

enum PhotoWriter {
  /// One context for the life of the process; creating one per photo is the
  /// expensive part of Core Image.
  private static let context = CIContext(options: [.cacheIntermediates: false])

  /**
   * Deep-copies a camera pixel buffer, so the ARFrame that owns the original
   * can go back to ARKit's pool at once (see DepthSnapshot).
   *
   * capturedImage is bi-planar YCbCr; every plane is copied row by row, since
   * the copy's row padding may differ from the source's.
   */
  static func copy(_ src: CVPixelBuffer) -> CVPixelBuffer? {
    var out: CVPixelBuffer?
    let attrs: [CFString: Any] = [kCVPixelBufferIOSurfacePropertiesKey: [:] as [CFString: Any]]
    guard
      CVPixelBufferCreate(
        kCFAllocatorDefault, CVPixelBufferGetWidth(src), CVPixelBufferGetHeight(src),
        CVPixelBufferGetPixelFormatType(src), attrs as CFDictionary, &out) == kCVReturnSuccess,
      let dst = out
    else { return nil }

    CVPixelBufferLockBaseAddress(src, .readOnly)
    CVPixelBufferLockBaseAddress(dst, [])
    defer {
      CVPixelBufferUnlockBaseAddress(dst, [])
      CVPixelBufferUnlockBaseAddress(src, .readOnly)
    }
    let planes = CVPixelBufferGetPlaneCount(src)
    if planes == 0 {
      guard let s = CVPixelBufferGetBaseAddress(src), let d = CVPixelBufferGetBaseAddress(dst) else { return nil }
      copyRows(s, CVPixelBufferGetBytesPerRow(src), d, CVPixelBufferGetBytesPerRow(dst), CVPixelBufferGetHeight(src))
    } else {
      for p in 0..<planes {
        guard let s = CVPixelBufferGetBaseAddressOfPlane(src, p),
          let d = CVPixelBufferGetBaseAddressOfPlane(dst, p)
        else { return nil }
        copyRows(
          s, CVPixelBufferGetBytesPerRowOfPlane(src, p), d, CVPixelBufferGetBytesPerRowOfPlane(dst, p),
          CVPixelBufferGetHeightOfPlane(src, p))
      }
    }
    return dst
  }

  private static func copyRows(
    _ s: UnsafeMutableRawPointer, _ sStride: Int, _ d: UnsafeMutableRawPointer, _ dStride: Int, _ rows: Int
  ) {
    let n = min(sStride, dStride)
    for r in 0..<rows { memcpy(d + r * dStride, s + r * sStride, n) }
  }

  /**
   * Writes `buffer` (sensor orientation) as an upright portrait JPEG.
   *
   * Upright in the PIXELS, turned 90 degrees clockwise (EXIF 6, `.right`), not
   * by an orientation tag: the photo goes on through the app's downscale and
   * to a vision model, and neither should have to honour a tag. It is encoded
   * from a CGImage, which carries no metadata at all, and GPS is excluded
   * explicitly besides -- nothing downstream needs to know where an item was
   * photographed.
   */
  static func writeJPEG(_ buffer: CVPixelBuffer, to url: URL, quality: Double = 0.9) throws {
    let image = CIImage(cvPixelBuffer: buffer).oriented(.right)
    guard let cg = context.createCGImage(image, from: image.extent) else {
      throw PhotoWriterError.encode("could not render the camera image")
    }
    guard
      let dest = CGImageDestinationCreateWithURL(url as CFURL, UTType.jpeg.identifier as CFString, 1, nil)
    else {
      throw PhotoWriterError.encode("could not create \(url.lastPathComponent)")
    }
    let options: [CFString: Any] = [
      kCGImageDestinationLossyCompressionQuality: quality,
      kCGImageMetadataShouldExcludeGPS: true,
    ]
    CGImageDestinationAddImage(dest, cg, options as CFDictionary)
    guard CGImageDestinationFinalize(dest) else {
      throw PhotoWriterError.encode("could not write \(url.lastPathComponent)")
    }
  }
}

enum PhotoWriterError: Error, CustomStringConvertible {
  case encode(String)

  var description: String {
    switch self {
    case .encode(let message): return message
    }
  }
}
