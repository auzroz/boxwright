//
//  HybridDepthCamera.swift
//  BoxwrightDepth
//
//  The camera screen's view: an ARKit session showing the camera, reporting
//  tilt and distance for the on-screen guide, and on capture() writing one
//  upright JPEG and, beside it, that frame's depth.
//
//  ARKit rather than AVFoundation, because an ARFrame brings everything the
//  measurement needs in one piece: depth aligned to the image, a per-pixel
//  confidence map, the intrinsics, and a camera pose that is gravity-aligned
//  -- which is what turns "where is the shelf" into a one-dimensional problem
//  (app/src/measure/).
//
//  Nothing here leaves the phone. The files go to the temporary directory,
//  the app moves them into its own documents directory, and the depth file is
//  deleted once the capture has been reviewed.
//

import ARKit
import AVFoundation
import Foundation
import NitroModules
import RealityKit
import UIKit

class HybridDepthCamera: HybridDepthCameraSpec {
  // MARK: Props

  /// Informational to the native side: the guide overlay is drawn in JS, and
  /// both modes want the same frames. Kept so a later mode can differ.
  var mode: DepthCameraMode = .item

  var active: Bool = false {
    didSet { if active != oldValue { applyActive() } }
  }

  var torch: Bool = false {
    didSet { if torch != oldValue { applyTorch() } }
  }

  var onStatus: ((_ status: DepthStatus) -> Void)?

  // MARK: View

  /// A plain container, so `view` never changes even though the AR view is
  /// only created on a phone that can use it.
  let view: UIView

  private var arView: ARView?
  private let statusRelay = StatusRelay()
  private var running = false

  override init() {
    // UIKit objects are made on the main thread, whichever thread Nitro
    // constructs this on (Fabric uses main; nothing here relies on it).
    view = HybridDepthCamera.mainSync {
      let v = UIView()
      v.backgroundColor = .black
      return v
    }
    super.init()
    statusRelay.owner = self
    // A phone without LiDAR never gets here from the app, which keeps the
    // system camera there; a black view is the safe answer if it ever does.
    guard
      ARWorldTrackingConfiguration.isSupported,
      ARWorldTrackingConfiguration.supportsFrameSemantics(.sceneDepth)
    else { return }
    HybridDepthCamera.mainSync { [self] in
      // RealityKit draws the camera feed and nothing else: there is no
      // content to render, so every effect it would spend the GPU on is off,
      // and it is never allowed to configure the session itself.
      let ar = ARView(frame: .zero, cameraMode: .ar, automaticallyConfigureSession: false)
      ar.renderOptions = [
        .disableMotionBlur, .disableDepthOfField, .disablePersonOcclusion,
        .disableGroundingShadows, .disableFaceMesh, .disableHDR, .disableCameraGrain,
        .disableAREnvironmentLighting,
      ]
      ar.environment.sceneUnderstanding.options = []
      ar.debugOptions = []
      ar.autoresizingMask = [.flexibleWidth, .flexibleHeight]
      ar.frame = self.view.bounds
      ar.session.delegate = self.statusRelay
      self.view.addSubview(ar)
      self.arView = ar
    }
  }

  private static func mainSync<T>(_ work: () -> T) -> T {
    Thread.isMainThread ? work() : DispatchQueue.main.sync(execute: work)
  }

  func onDropView() {
    onMain { self.pause() }
  }

  func dispose() {
    onMain { self.pause() }
  }

  // MARK: Session

  private func applyActive() {
    onMain { self.active ? self.run() : self.pause() }
  }

  /**
   * World tracking with depth: smoothed where the phone offers it (averaged
   * over a few frames, far fewer flying pixels at edges), raw otherwise.
   * Horizontal planes, for the measurement's fallback when the surface under
   * an item gives no depth of its own -- black, glossy. A 4:3 video format, so
   * the image and the 256x192 depth map cover the same field of view and the
   * intrinsics scale between them exactly.
   */
  private func run() {
    guard let ar = arView, !running else { return }
    let config = ARWorldTrackingConfiguration()
    if ARWorldTrackingConfiguration.supportsFrameSemantics(.smoothedSceneDepth) {
      config.frameSemantics = [.smoothedSceneDepth]
    } else {
      config.frameSemantics = [.sceneDepth]
    }
    config.planeDetection = [.horizontal]
    config.environmentTexturing = .none
    if let format = HybridDepthCamera.fourByThreeFormat() { config.videoFormat = format }
    ar.session.run(config, options: [.resetTracking, .removeExistingAnchors])
    running = true
    applyTorch()
  }

  /// Paused whenever the screen is not showing: no camera, no LiDAR, no heat.
  private func pause() {
    guard running, let ar = arView else { return }
    ar.session.pause()
    running = false
  }

  /// The largest 4:3 format, which on current Pro phones is 1920x1440.
  private static func fourByThreeFormat() -> ARConfiguration.VideoFormat? {
    ARWorldTrackingConfiguration.supportedVideoFormats
      .filter { abs($0.imageResolution.width * 3 - $0.imageResolution.height * 4) < 1 }
      .max { $0.imageResolution.width < $1.imageResolution.width }
  }

  /**
   * The torch is the camera's, not ARKit's: ARKit hands over the capture
   * device it is using from iOS 16, and before that there is no safe way to
   * reach it while a session runs, so the prop does nothing there.
   */
  private func applyTorch() {
    guard running else { return }
    if #available(iOS 16.0, *) {
      guard let device = ARWorldTrackingConfiguration.configurableCaptureDeviceForPrimaryCamera,
        device.hasTorch
      else { return }
      do {
        try device.lockForConfiguration()
        device.torchMode = torch ? .on : .off
        device.unlockForConfiguration()
      } catch {
        // Best effort: a torch that will not turn on costs light, not a capture.
      }
    }
  }

  // MARK: Status

  /// Called by the relay on the session's delegate queue (main).
  fileprivate func didUpdate(_ frame: ARFrame) {
    guard let onStatus = onStatus, statusRelay.due(at: frame.timestamp) else { return }
    onStatus(HybridDepthCamera.status(of: frame))
  }

  /**
   * What the capture screen shows while aiming. Computed from the frame and
   * handed on as plain numbers; the frame itself is not kept.
   *
   * Tilt: the camera looks along -z of its transform, so the angle between
   * that and straight down (0, -1, 0) has cosine column2.y.
   */
  private static func status(of frame: ARFrame) -> DepthStatus {
    let tracking: DepthTracking
    switch frame.camera.trackingState {
    case .normal: tracking = .normal
    case .limited: tracking = .limited
    case .notAvailable: tracking = .notavailable
    }
    let c2 = frame.camera.transform.columns.2
    let tilt = Double(acos(max(-1, min(1, c2.y)))) * 180 / .pi
    let centre = centreDepth(of: frame)
    return DepthStatus(
      tracking: tracking,
      tiltFromDownDeg: (tilt * 10).rounded() / 10,
      centerDistanceM: centre,
      depthOK: centre != nil
    )
  }

  /// Depth at the middle of the frame, in metres, if it is a usable reading.
  private static func centreDepth(of frame: ARFrame) -> Double? {
    guard let depth = frame.smoothedSceneDepth ?? frame.sceneDepth else { return nil }
    let map = depth.depthMap
    guard CVPixelBufferGetPixelFormatType(map) == kCVPixelFormatType_DepthFloat32 else { return nil }
    let x = CVPixelBufferGetWidth(map) / 2
    let y = CVPixelBufferGetHeight(map) / 2
    CVPixelBufferLockBaseAddress(map, .readOnly)
    defer { CVPixelBufferUnlockBaseAddress(map, .readOnly) }
    guard let base = CVPixelBufferGetBaseAddress(map) else { return nil }
    let metres = (base + y * CVPixelBufferGetBytesPerRow(map)).assumingMemoryBound(to: Float32.self)[x]
    if let cmap = depth.confidenceMap {
      CVPixelBufferLockBaseAddress(cmap, .readOnly)
      defer { CVPixelBufferUnlockBaseAddress(cmap, .readOnly) }
      if let cbase = CVPixelBufferGetBaseAddress(cmap) {
        let conf = (cbase + y * CVPixelBufferGetBytesPerRow(cmap)).assumingMemoryBound(to: UInt8.self)[x]
        if conf < UInt8(ARConfidenceLevel.medium.rawValue) { return nil }
      }
    }
    // The same range the measurement accepts (app/src/measure/frame.ts).
    guard metres.isFinite, metres >= 0.15, metres <= 4 else { return nil }
    return (Double(metres) * 100).rounded() / 100
  }

  // MARK: Capture

  private static let encodeQueue = DispatchQueue(label: "app.boxwright.depth.encode", qos: .userInitiated)

  /**
   * Writes the current frame's photo, and its depth when it has any.
   *
   * The frame is read on the main thread and copied out of at once -- the
   * image into a fresh pixel buffer, the depth into plain arrays -- so it goes
   * back to ARKit before the slow part, the JPEG encode, runs elsewhere.
   * A frame with no depth still gives a photo: the capture is never lost
   * because the measurement could not be taken.
   */
  func capture() throws -> Promise<DepthCapture> {
    let promise = Promise<DepthCapture>()
    onMain {
      guard self.running, let frame = self.arView?.session.currentFrame else {
        promise.reject(withError: RuntimeError("The camera is not running."))
        return
      }
      let image = PhotoWriter.copy(frame.capturedImage)
      let snapshot = DepthSnapshot.copy(from: frame)
      guard let image = image else {
        promise.reject(withError: RuntimeError("Could not copy the camera image."))
        return
      }
      HybridDepthCamera.encodeQueue.async {
        do {
          let id = UUID().uuidString
          let dir = FileManager.default.temporaryDirectory
          let photo = dir.appendingPathComponent("\(id).jpg")
          try PhotoWriter.writeJPEG(image, to: photo)
          var depthPath: String?
          if let snapshot = snapshot {
            let url = dir.appendingPathComponent("\(id).depth")
            // Written atomically: a half-written file is refused by the reader,
            // but better never to leave one.
            try snapshot.encoded().write(to: url, options: .atomic)
            depthPath = url.path
          }
          promise.resolve(withResult: DepthCapture(photoPath: photo.path, depthPath: depthPath))
        } catch {
          promise.reject(withError: RuntimeError("Could not save the capture: \(error)"))
        }
      }
    }
    return promise
  }

  private func onMain(_ work: @escaping () -> Void) {
    if Thread.isMainThread { work() } else { DispatchQueue.main.async(execute: work) }
  }
}

/**
 * The session's delegate, kept separate so the view object need not be an
 * NSObject, and so ARKit holds it weakly without holding the view.
 */
private final class StatusRelay: NSObject, ARSessionDelegate {
  weak var owner: HybridDepthCamera?
  private var last: TimeInterval = -.infinity

  /// About five updates a second: enough for a tilt meter to feel live,
  /// far below the 60 frames a second ARKit delivers. Timed by the frame's
  /// own timestamp rather than a system clock: no uptime API to declare in
  /// the privacy manifest.
  func due(at timestamp: TimeInterval) -> Bool {
    if timestamp - last < 0.2 { return false }
    last = timestamp
    return true
  }

  func session(_ session: ARSession, didUpdate frame: ARFrame) {
    owner?.didUpdate(frame)
  }
}
