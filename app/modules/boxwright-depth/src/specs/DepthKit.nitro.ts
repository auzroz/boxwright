import type { HybridObject } from 'react-native-nitro-modules'

/** AVFoundation's camera authorisation, as it stands before asking. */
export type CameraAccess = 'granted' | 'denied' | 'restricted' | 'undetermined'

/**
 * What the app asks of the LiDAR side outside the camera view itself.
 *
 * iOS only: Boxwright ships no Android app, and a spec typed for iOS alone
 * keeps nitrogen from generating Kotlin nobody builds.
 */
export interface DepthKit extends HybridObject<{ ios: 'swift' }> {
  /**
   * Whether this phone can capture scene depth at all -- a LiDAR iPhone Pro.
   * Everything else keeps the system camera, unchanged.
   */
  readonly isSupported: boolean
  /**
   * Whether the camera may be used. Read before opening Boxwright's own
   * camera: ARKit given no access fails silently into a black preview, while
   * the system camera explains itself and offers Settings.
   */
  readonly cameraAccess: CameraAccess
  /**
   * Reads a whole file into memory. Used for `.depth` files (about 145 KB),
   * which react-native-file-access can only hand over as base64.
   */
  readFile(path: string): Promise<ArrayBuffer>
}
