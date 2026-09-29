import type { HybridObject } from 'react-native-nitro-modules'

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
   * Reads a whole file into memory. Used for `.depth` files (about 145 KB),
   * which react-native-file-access can only hand over as base64.
   */
  readFile(path: string): Promise<ArrayBuffer>
}
