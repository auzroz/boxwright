/**
 * A stand-in for the local boxwright-depth module, mapped in for tests.
 *
 * The real one creates a Nitro HybridObject and registers a Fabric view, both
 * native. This mirrors its exports (src/index.ts) exactly -- a test in this
 * directory checks the names -- and its types come from the real specs, so a
 * change to the native contract breaks the typecheck here rather than passing
 * silently against a fake that no longer matches.
 *
 * Unsupported by default, which is what every phone without LiDAR and the
 * Simulator report: the app must keep working on the system camera.
 */
import type {
  DepthCameraMethods,
  DepthCameraProps,
} from "../modules/boxwright-depth/src/specs/DepthCamera.nitro";
import type { DepthKit as DepthKitSpec } from "../modules/boxwright-depth/src/specs/DepthKit.nitro";

export type {
  DepthCameraMethods,
  DepthCameraMode,
  DepthCameraProps,
  DepthCapture,
  DepthStatus,
  DepthTracking,
} from "../modules/boxwright-depth/src/specs/DepthCamera.nitro";
export type { DepthCamera as DepthCameraRef } from "../modules/boxwright-depth/src/specs/DepthCamera.nitro";

let supported = false;
/** path -> file contents, for readFile. */
export const depthFiles = new Map<string, Uint8Array>();

/** Back to an unsupported phone with no files. */
export function resetDepth(opts: { supported?: boolean } = {}): void {
  supported = opts.supported ?? false;
  depthFiles.clear();
}

export const DepthKit: Pick<DepthKitSpec, "isSupported" | "readFile"> = {
  get isSupported(): boolean {
    return supported;
  },
  async readFile(path: string): Promise<ArrayBuffer> {
    const bytes = depthFiles.get(path);
    // Same shape of failure as the native side: a missing file rejects.
    if (!bytes) throw new Error(`The file "${path}" couldn't be opened because there is no such file.`);
    return bytes.slice().buffer as ArrayBuffer;
  },
};

/**
 * The view, as far as a test can see it: its name and its props. Nothing
 * renders under the node test environment, so it only needs to exist.
 */
export function DepthCamera(_props: DepthCameraProps & { hybridRef?: unknown }): null {
  return null;
}
DepthCamera.displayName = "DepthCamera";

/** react-native-nitro-modules' callback(): a function is wrapped as { f }. */
export function callback<T>(func: T): T extends (...args: never[]) => unknown ? { f: T } : T {
  return (typeof func === "function" ? { f: func } : func) as T extends (...args: never[]) => unknown ? { f: T } : T;
}

/** A capture() result a test can hand to code under test. */
export type FakeCapture = Awaited<ReturnType<DepthCameraMethods["capture"]>>;
