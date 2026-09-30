import type {
  HybridView,
  HybridViewMethods,
  HybridViewProps,
} from 'react-native-nitro-modules'

/**
 * `item` frames one thing to be identified and sized; `fill` looks straight
 * down into an open container to see how full it is.
 */
export type DepthCameraMode = 'item' | 'fill'

/** ARKit's tracking state, flattened: `limited` covers all of its reasons. */
export type DepthTracking = 'normal' | 'limited' | 'notAvailable'

/** Live guidance for the capture screen, sent about five times a second. */
export interface DepthStatus {
  tracking: DepthTracking
  /** How far the camera's view is from pointing straight down, in degrees. */
  tiltFromDownDeg: number
  /** Depth at the middle of the frame in metres, when there is a reading. */
  centerDistanceM?: number
  /** Whether the latest frame carried usable depth. */
  depthOK: boolean
}

/**
 * `failed`: the session stopped and will not restart by itself -- camera
 * access refused, the camera unavailable, an unsupported configuration.
 * `interrupted`: something else took the camera (a call, Control Centre, the
 * app going to the background), and `resumed` when it is back.
 */
export type DepthSessionState = 'failed' | 'interrupted' | 'resumed'

/** Sent when the session fails or is interrupted: the cases no frame reports. */
export interface DepthSessionEvent {
  state: DepthSessionState
  /** ARKit's own description, for a failure. */
  message?: string
  /** The failure was camera access being refused, so Settings is the fix. */
  cameraDenied: boolean
}

/** What one capture wrote, both in the temporary directory. */
export interface DepthCapture {
  /** An upright JPEG with no EXIF or GPS metadata. */
  photoPath: string
  /** The frame's `.depth` file (BWD1); absent when the frame had no depth. */
  depthPath?: string
}

export interface DepthCameraProps extends HybridViewProps {
  mode: DepthCameraMode
  /** False pauses the AR session: no camera, no sensors, no battery. */
  active: boolean
  torch: boolean
  onStatus?: (status: DepthStatus) => void
  onSessionEvent?: (event: DepthSessionEvent) => void
}

export interface DepthCameraMethods extends HybridViewMethods {
  capture(): Promise<DepthCapture>
}

export type DepthCamera = HybridView<
  DepthCameraProps,
  DepthCameraMethods,
  { ios: 'swift' }
>
