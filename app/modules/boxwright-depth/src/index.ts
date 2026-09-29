import {
  NitroModules,
  callback,
  getHostComponent,
} from 'react-native-nitro-modules'

import DepthCameraConfig from '../nitrogen/generated/shared/json/DepthCameraConfig.json'
import type {
  DepthCamera as DepthCameraSpec,
  DepthCameraMethods,
  DepthCameraProps,
} from './specs/DepthCamera.nitro'
import type { DepthKit as DepthKitSpec } from './specs/DepthKit.nitro'

export type {
  DepthCameraMethods,
  DepthCameraMode,
  DepthCameraProps,
  DepthCapture,
  DepthStatus,
  DepthTracking,
} from './specs/DepthCamera.nitro'

/** What `hybridRef` hands back: call `capture()` on it. */
export type DepthCameraRef = DepthCameraSpec

let kit: DepthKitSpec | null | undefined

/**
 * The native object, created on first use rather than at import, so a build
 * without the native side (a test, a platform we do not ship) can still
 * import this file and simply finds depth unsupported.
 */
function nativeKit(): DepthKitSpec | null {
  if (kit === undefined) {
    try {
      kit = NitroModules.createHybridObject<DepthKitSpec>('DepthKit')
    } catch {
      kit = null
    }
  }
  return kit
}

export const DepthKit = {
  /** False on any phone without LiDAR, and wherever the native side is missing. */
  get isSupported(): boolean {
    return nativeKit()?.isSupported ?? false
  },
  /** A whole file, e.g. a `.depth`, as bytes. */
  readFile(path: string): Promise<ArrayBuffer> {
    const k = nativeKit()
    if (!k) return Promise.reject(new Error('Depth capture is not available on this device.'))
    return k.readFile(path)
  },
}

/**
 * The camera view. Function props must be wrapped: `onStatus={callback(fn)}`
 * and `hybridRef={callback((ref) => ...)}` (a React Native limitation, see
 * react-native-nitro-modules' `callback`).
 */
export const DepthCamera = getHostComponent<DepthCameraProps, DepthCameraMethods>(
  'DepthCamera',
  () => DepthCameraConfig
)

export { callback }
