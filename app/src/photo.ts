import ImageEditor from "@react-native-community/image-editor";
import { Image } from "react-native";
import { Dirs, FileSystem } from "react-native-file-access";

/**
 * A 4032x3024 iPhone capture is about 16k vision tokens; the same photo at
 * 1024px on its longest edge is about 1400. Nothing in identification needs
 * the extra pixels -- we are naming a drill, not reading a serial number --
 * and the difference is roughly 10x the cost of every single capture. It also
 * makes an offline queue entry a couple of hundred kilobytes on disk and a
 * few seconds rather than a few minutes to flush over one bar of signal.
 */
const LONGEST_EDGE = 1024;
/** 0-1. 0.6 is where JPEG artefacts stop mattering to a vision model. */
const JPEG_QUALITY = 0.6;

/** Where durable capture photos live, relative to the document directory. */
const CAPTURES_DIR = "captures";

/** An image's size in pixels, for a file: or a data: uri alike. */
function measure(uri: string): Promise<{ width: number; height: number }> {
  return new Promise((resolve, reject) => {
    Image.getSize(uri, (width, height) => resolve({ width, height }), reject);
  });
}

/**
 * The size to re-encode a `width` x `height` image at: at most LONGEST_EDGE on
 * its longest side, aspect ratio kept, and never scaled UP.
 */
export function downscaledSize(width: number, height: number): { width: number; height: number } {
  const scale = Math.min(1, LONGEST_EDGE / Math.max(width, height));
  return { width: Math.max(1, Math.round(width * scale)), height: Math.max(1, Math.round(height * scale)) };
}

/**
 * Re-encodes an image to at most LONGEST_EDGE on its longest side, as JPEG,
 * returning the path of the new file (in the cache directory).
 *
 * The editor is a cropper that can also scale, so this is a crop of the WHOLE
 * image to a smaller display size; it needs the source's size, which is why
 * the image is measured first. Small images are still re-saved rather than
 * passed through: the source may be a HEIC or a PNG, and it is the re-encode,
 * not only the resize, that gets the file down to something worth uploading
 * over bad signal. The re-encode (UIImageJPEGRepresentation) also carries no
 * EXIF over: it holds GPS coordinates, and nothing downstream needs to know
 * where a drill was photographed.
 *
 * It replaced @bam.tech/react-native-image-resizer, whose podspec depends on
 * the RCT-Folly and React-Codegen pods that React Native 0.86 no longer has,
 * so `pod install` could not resolve it.
 */
export async function downscale(uri: string): Promise<string> {
  const { width, height } = await measure(uri);
  if (!(width > 0) || !(height > 0)) throw new Error(`could not measure the image (${width}x${height})`);
  const result = await ImageEditor.cropImage(uri, {
    offset: { x: 0, y: 0 },
    size: { width, height },
    displaySize: downscaledSize(width, height),
    // The display size already has the source's aspect ratio, so filling it
    // exactly distorts nothing.
    resizeMode: "stretch",
    quality: JPEG_QUALITY,
    format: "jpeg",
  });
  return result.path;
}

/** react-native-file-access takes plain paths; the picker and Image speak file:// uris. */
function toPath(uri: string): string {
  return uri.startsWith("file://") ? decodeURI(uri.slice("file://".length)) : uri;
}

function toUri(path: string): string {
  return `file://${encodeURI(path)}`;
}

/**
 * The captures directory, resolved at call time.
 *
 * Queue entries record a NAME, never an absolute path, and this is where the
 * two are joined back together. iOS hands the app a fresh container UUID on
 * reinstall and on some updates, so a `/var/.../Application/<uuid>/Documents/
 * captures/x.jpg` written down today can dangle tomorrow while the file itself
 * is still perfectly present. `Dirs.DocumentDir` is always current.
 */
function capturesPath(name?: string): string {
  const dir = `${Dirs.DocumentDir}/${CAPTURES_DIR}`;
  return name === undefined ? dir : `${dir}/${name}`;
}

async function ensureCapturesDirectory(): Promise<string> {
  const dir = capturesPath();
  if (!(await FileSystem.exists(dir))) await FileSystem.mkdir(dir);
  return dir;
}

/** Resolves a stored photo name to a uri, or null if the file is gone. */
export async function capturePhotoUri(name: string | undefined): Promise<string | null> {
  if (!name) return null;
  const path = capturesPath(name);
  try {
    return (await FileSystem.exists(path)) ? toUri(path) : null;
  } catch {
    return null;
  }
}

/**
 * The uri a stored photo name would have, without checking it is still there.
 *
 * For render paths, which cannot wait on the existence check: an <Image> of a
 * file that has gone simply draws nothing. Anything that uploads the photo
 * uses capturePhotoUri instead.
 */
export function storedPhotoUri(name: string): string {
  return toUri(capturesPath(name));
}

export interface PersistedPhoto {
  /** Name within the captures directory; this is what is safe to persist. */
  name: string;
  /** Uri valid for this app launch, for display and upload. */
  uri: string;
  /** False when downscaling failed and the original was kept instead. */
  downscaled: boolean;
}

/**
 * Moves a freshly captured photo somewhere it will survive an app restart,
 * downscaling it on the way in.
 *
 * The single intake point for every source: the camera, the photo library, and
 * a pasted image. Whatever a capture came from, it is one downscaled JPEG in
 * the document directory by the time anything else sees it -- which is why the
 * offline queue, the crop, the upload and the identify call need to know
 * nothing about where it came from.
 *
 * The picker hands back a file in the temporary directory, which iOS is free
 * to clear and which is not guaranteed to outlive the process. A queued
 * capture whose photo has gone is a half-lost capture, so every capture is
 * promoted to the document directory up front -- before we know whether it
 * will need to be queued -- and deleted again once it has been filed.
 *
 * A downscale failure is never fatal: uploading a large photo is far better
 * than losing the capture.
 */
export async function persistCapturePhoto(captureId: string, sourceUri: string): Promise<PersistedPhoto> {
  const dir = await ensureCapturesDirectory();
  try {
    const name = `${captureId}.jpg`;
    // The editor's output is ours alone, so it is moved rather than copied.
    await FileSystem.mv(await downscale(sourceUri), `${dir}/${name}`);
    return { name, uri: toUri(`${dir}/${name}`), downscaled: true };
  } catch (err) {
    // Keeping the original is only possible when the original is a FILE. A
    // pasted image is a data: uri -- bytes in a string, which only the image
    // editor can turn into a file -- so there is nothing to fall back to, and
    // pretending otherwise would produce a capture whose photo fails at upload.
    if (!isFileUri(sourceUri)) {
      throw new Error(`could not decode the pasted image: ${err instanceof Error ? err.message : String(err)}`);
    }
    // Keep the original. Copied, not moved: the picker's file is its own. It
    // keeps its own extension too, because the upload's content type is read
    // from it (api.imageTypeFor), and a HEIC labelled as a JPEG is refused.
    const ext = /\.([a-z0-9]+)$/i.exec(toPath(sourceUri))?.[1]?.toLowerCase() ?? "jpg";
    const name = `${captureId}.${ext}`;
    await FileSystem.cp(toPath(sourceUri), `${dir}/${name}`);
    return { name, uri: toUri(`${dir}/${name}`), downscaled: false };
  }
}

/**
 * Moves a LiDAR depth file from wherever the native camera wrote it into the
 * captures directory, as `<captureId>.depth`, returning the NAME to persist.
 *
 * Beside the photo for the same reason the photo is there: the native side
 * writes to the temporary directory, which iOS may clear, and a parked capture
 * can wait hours for review. Being in captures/ also puts it under the startup
 * GC, so one abandoned mid-capture cannot leak ~145 KB forever -- which is why
 * the pending list has to name it (offline.pendingPhotoNames).
 *
 * Moved, not copied: the file is the camera's output for this capture alone.
 */
export async function persistCaptureDepth(captureId: string, tmpPath: string): Promise<string> {
  const dir = await ensureCapturesDirectory();
  const name = `${captureId}.depth`;
  await FileSystem.mv(toPath(tmpPath), `${dir}/${name}`);
  return name;
}

/**
 * Whether a uri names something on disk that can be copied.
 *
 * `file:` covers the camera and the library picker. A `data:` uri from the
 * clipboard is bytes in a string, and only the image editor can turn it into a
 * file.
 */
export function isFileUri(uri: string): boolean {
  return uri.startsWith("file:");
}

/**
 * Deletes files the camera left in the temporary directory that nothing will
 * keep: a fill photo and its depth, once measured. Best effort and never
 * waited on -- iOS clears the temporary directory eventually anyway; this only
 * keeps depth maps on the phone no longer than they are needed.
 */
export function discardTempFiles(...paths: (string | undefined)[]): void {
  for (const path of paths) {
    if (path) void FileSystem.unlink(toPath(path)).catch(() => {});
  }
}

/**
 * Deletes one stored photo -- or any other file in captures/, such as a depth
 * file. Safe to call when it is already gone, or with no name; never waits.
 */
export function deleteCapturePhoto(name: string | undefined): void {
  if (!name) return;
  // Best effort: a stale photo costs disk, never correctness.
  void FileSystem.unlink(capturesPath(name)).catch(() => {});
}

/**
 * Deletes stored photos (and depth files) that no queue entry references.
 *
 * Run at startup only. A capture that was persisted and then abandoned (the
 * app was killed during review, the user backed out) otherwise leaks a file
 * forever. Anything still referenced by the queue is kept, so this can never
 * take a photo away from a capture that has not been filed yet.
 *
 * It runs in the background, and that is safe for one reason worth keeping
 * true: only files present when the directory is LISTED are candidates, and at
 * launch every such file is either queued or orphaned. A capture in review
 * does not survive a restart, and the camera cannot have produced a new one
 * in the moment between launch and the listing.
 */
export function gcCapturePhotos(referenced: Set<string>): void {
  void (async () => {
    const dir = capturesPath();
    if (!(await FileSystem.exists(dir))) return;
    for (const name of await FileSystem.ls(dir)) {
      if (!referenced.has(name)) await FileSystem.unlink(`${dir}/${name}`).catch(() => {});
    }
  })().catch(() => {
    /* best effort */
  });
}
