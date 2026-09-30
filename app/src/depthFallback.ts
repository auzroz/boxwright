// What to do when Boxwright's own (LiDAR) camera cannot do its job: which
// camera to open at all, and what the camera screen says and offers while it
// is failing to start, has failed, or is struggling. Pure, so every branch is
// tested; DepthCaptureModal only draws the answer.
//
// The rule underneath all of it: a photo never depends on LiDAR. Anything
// that goes wrong with depth leaves a way to take the photo anyway, or -- for
// measuring fill -- to say how full it is by hand.

import type { CameraAccess, DepthCameraMode, DepthSessionEvent, DepthStatus } from "boxwright-depth";

/** Without a first frame by now, the camera is not starting. */
export const START_TIMEOUT_MS = 8000;
/** Tracking this long without settling is worth a suggestion. */
export const POOR_TRACKING_MS = 10000;

/**
 * Which camera "Take photo" opens. Ours only where there is depth to keep
 * AND access is not known to be refused: ARKit refused the camera shows a
 * black preview and says nothing, while the system camera explains itself and
 * offers Settings.
 */
export function cameraFor(kit: { isSupported: boolean; cameraAccess: CameraAccess }): "depth" | "system" {
  if (!kit.isSupported) return "system";
  return kit.cameraAccess === "denied" || kit.cameraAccess === "restricted" ? "system" : "depth";
}

export interface DepthScreen {
  /** The line over the preview. */
  hint: string;
  /** The session has stopped for good: show `problem` in place of the shutter. */
  failed: boolean;
  problem?: string;
  /** Camera access is the cause, so Settings is the fix. */
  openSettings: boolean;
  /** Offer the way round: the system camera (item) or answering by hand (fill). */
  offerFallback: boolean;
  /** Whether the shutter can be pressed. */
  canShoot: boolean;
}

export function depthScreen(input: {
  mode: DepthCameraMode;
  status: DepthStatus | null;
  /** The latest session event, if any since opening. */
  event: DepthSessionEvent | null;
  /** When the camera was opened, and now, in ms. */
  openedAt: number;
  now: number;
  /** Since when tracking has been other than normal, or null while it is normal. */
  poorSince: number | null;
}): DepthScreen {
  const fill = input.mode === "fill";
  const { status, event } = input;

  if (event?.state === "failed") {
    return {
      hint: "",
      failed: true,
      problem: event.cameraDenied
        ? "Boxwright isn’t allowed to use the camera. Turn it on in Settings."
        : `The camera stopped${event.message ? `: ${event.message}` : "."}`,
      openSettings: event.cameraDenied,
      offerFallback: true,
      canShoot: false,
    };
  }
  if (event?.state === "interrupted") {
    return {
      hint: "Something else is using the camera. It comes back when that ends.",
      failed: false,
      openSettings: false,
      offerFallback: true,
      canShoot: false,
    };
  }
  if (!status) {
    const slow = input.now - input.openedAt >= START_TIMEOUT_MS;
    return {
      hint: slow ? "The camera hasn’t started." : "Starting the camera…",
      failed: false,
      openSettings: false,
      offerFallback: slow,
      canShoot: false,
    };
  }
  const struggling = input.poorSince !== null && input.now - input.poorSince >= POOR_TRACKING_MS;
  if (status.tracking !== "normal") {
    return {
      hint: struggling
        ? fill
          ? "It can’t find its bearings here. More light may help, or choose how full it is instead."
          : "It can’t find its bearings here. More light may help. The photo still works without depth."
        : "Move the phone slowly so it can find its bearings.",
      failed: false,
      openSettings: false,
      offerFallback: struggling,
      // A photo without depth is still a photo; a fill reading is not.
      canShoot: !fill,
    };
  }
  if (fill) {
    const tilt = Math.round(status.tiltFromDownDeg);
    return {
      hint: tilt > 30 ? `Tilted ${tilt}°. Hold it flatter, looking straight down.` : "Frame the open container inside the box, looking straight down.",
      failed: false,
      openSettings: false,
      offerFallback: true,
      canShoot: true,
    };
  }
  return {
    hint: status.depthOK ? "Ready." : "Too close or too far to measure. The photo still works.",
    failed: false,
    openSettings: false,
    offerFallback: true,
    canShoot: true,
  };
}
