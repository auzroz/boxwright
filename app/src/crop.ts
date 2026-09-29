import type { Region } from "./types";

/**
 * How much context to keep around a region, as a fraction of its size.
 *
 * A box drawn tight to an object is often harder to recognise than the object
 * in its surroundings -- a cable coiled on a shelf reads as a cable; the same
 * cable cropped to its own outline reads as a grey smear. This is for a person
 * looking at a thumbnail, not for a model.
 */
export const CROP_PADDING = 0.12;

/** A rectangle in the source photo's pixels. */
export interface CropRect {
  x: number;
  y: number;
  width: number;
  height: number;
}

/**
 * The part of a `width` x `height` photo that shows one item, padded -- or null
 * when there is nothing better to show than the whole photo.
 *
 * Null is a completely ordinary answer, not a failure. Most models do not
 * produce a region at all, and every caller already has the full photo to fall
 * back on.
 *
 * The crop is drawn, never written: a crop is a picture of something already on
 * disk, needed only while the review screen is up, and a clipped view of the
 * one photo costs no native module, no file and no decode per item.
 */
export function cropRect(region: Region | undefined, width: number, height: number): CropRect | null {
  if (!region || !(width > 0) || !(height > 0)) return null;

  const padX = region.w * CROP_PADDING;
  const padY = region.h * CROP_PADDING;
  const x0 = Math.max(0, region.x - padX);
  const y0 = Math.max(0, region.y - padY);
  const x1 = Math.min(1, region.x + region.w + padX);
  const y1 = Math.min(1, region.y + region.h + padY);
  if (!(x1 > x0) || !(y1 > y0)) return null;

  // Whole pixels, floored at 1 in each direction, and never past the far edge.
  const x = Math.round(x0 * width);
  const y = Math.round(y0 * height);
  const w = Math.max(1, Math.min(width - x, Math.round((x1 - x0) * width)));
  const h = Math.max(1, Math.min(height - y, Math.round((y1 - y0) * height)));

  // A crop that is the whole photo is just the photo.
  if (w >= width && h >= height) return null;
  return { x, y, width: w, height: h };
}

/**
 * Where to draw a `photo`-sized image inside a `side` x `side` clipping box so
 * that exactly `crop` fills it, centred, the way resizeMode "cover" would fill
 * the box with an image that had been cropped on disk.
 */
export function coverCrop(
  crop: CropRect,
  photo: { width: number; height: number },
  side: number,
): { width: number; height: number; left: number; top: number } {
  const scale = Math.max(side / crop.width, side / crop.height);
  return {
    width: photo.width * scale,
    height: photo.height * scale,
    left: -crop.x * scale - (crop.width * scale - side) / 2,
    top: -crop.y * scale - (crop.height * scale - side) / 2,
  };
}
