// Where a LiDAR depth frame meets the rest of the app: sizes for the items
// identified in a photo, and the fill of a container photographed from above.
// The measuring itself is in src/measure/, which knows nothing of files or
// React Native; this reads the file and decides which answers to trust.

import { DepthKit } from "boxwright-depth";

import { decodeDepth } from "./measure/depthfile";
import { measureItem } from "./measure/dimensions";
import { measureFill } from "./measure/fill";
import type { FillResult } from "./measure/fill";
import type { Dims, ItemDraft, Region } from "./types";

/** Below this a measurement is worse than the model's estimate from what the object is. */
const MIN_CONFIDENCE = 0.5;

/**
 * Where the fill camera asks for the container to be framed, as fractions of
 * the upright preview. The camera screen draws exactly this rectangle, so what
 * the user lines up is what is measured.
 */
export const FILL_GUIDE: Region = { x: 0.1, y: 0.2, w: 0.8, h: 0.55 };

type ReadFile = (path: string) => Promise<ArrayBuffer>;
const readDepth: ReadFile = (path) => DepthKit.readFile(path);

/**
 * The identified items, with a measured size wherever the depth frame gives a
 * trustworthy one.
 *
 * With one item the measurement is of whatever the photo is of, region or
 * not. With several, only an item the model located is measured: a
 * whole-frame measurement is of SOMETHING in the photo, and would be pinned on
 * every unlocated item alike. Anything that fails -- an unreadable file, a
 * frame with too little in it -- leaves the item as the model described it.
 * Bulky things are not measured: their size decides nothing.
 */
export async function attachLidarDimensions(
  items: ItemDraft[],
  depthPath: string,
  read: ReadFile = readDepth,
): Promise<ItemDraft[]> {
  let frame;
  try {
    frame = decodeDepth(await read(depthPath));
  } catch {
    return items;
  }
  const single = items.length === 1;
  return items.map((item) => {
    if (item.bulky) return item;
    const region = item.region ?? undefined;
    if (!single && !region) return item;
    let size;
    try {
      size = measureItem(frame, region);
    } catch {
      return item;
    }
    if (!size || size.confidence < MIN_CONFIDENCE) return item;
    return { ...item, dimensionsCm: { l: size.l, w: size.w, h: size.h }, dimensionsSource: "lidar" };
  });
}

/** How full the container in the fill guide is, or why it could not say. */
export async function measureFillFromFile(
  depthPath: string,
  interior: Dims,
  read: ReadFile = readDepth,
): Promise<FillResult> {
  try {
    return measureFill(decodeDepth(await read(depthPath)), FILL_GUIDE, interior);
  } catch {
    return { refused: "The depth photo could not be read. Try again, or choose how full it is." };
  }
}
