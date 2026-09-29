//
//  HybridDepthKit.swift
//  BoxwrightDepth
//

import ARKit
import Foundation
import NitroModules

class HybridDepthKit: HybridDepthKitSpec {
  /**
   * Scene depth exists only on phones with a LiDAR scanner. `.sceneDepth`
   * rather than `.smoothedSceneDepth` because it is the weaker requirement:
   * the camera takes smoothed depth where it can and falls back to this.
   * False in the Simulator, which is what keeps the system camera there.
   */
  var isSupported: Bool {
    ARWorldTrackingConfiguration.isSupported
      && ARWorldTrackingConfiguration.supportsFrameSemantics(.sceneDepth)
  }

  func readFile(path: String) throws -> Promise<ArrayBuffer> {
    Promise.async {
      let url =
        path.hasPrefix("file://")
        ? (URL(string: path) ?? URL(fileURLWithPath: path))
        : URL(fileURLWithPath: path)
      let data = try Data(contentsOf: url)
      return try ArrayBuffer.copy(data: data)
    }
  }
}
