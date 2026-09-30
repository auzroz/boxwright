// How much room an item takes and whether a container can take it -- the
// app's mirror of backend/internal/placement/capacity.go and canHold in
// engine.go, for the offline picker and for what the screens say about fill.
//
// The rule is the engine's: only FACTS exclude. A recorded capacity, a
// measured size against a recorded interior, a fill someone observed. An
// unknown never excludes, and neither does an estimate. If this and the engine
// disagree, the engine is right and this is the bug.

import type { Box, Dims, ItemDraft, SizeBucket } from "./types";

/** Litres an item of each bucket is taken to occupy; sent by /boxes. */
export type SizeLitres = Partial<Record<SizeBucket, number>>;

const MIN_DIM_CM = 0.5;
const MAX_DIM_CM = 300;

/**
 * A believable size, longest side first, or undefined. Mirrors
 * placement.Dims.Normalize: an unbelievable size is dropped, never repaired.
 */
export function normalizeDims(d: Dims | null | undefined): Dims | undefined {
  if (!d) return undefined;
  const sides = [d.l, d.w, d.h];
  if (sides.some((v) => typeof v !== "number" || !Number.isFinite(v) || v < MIN_DIM_CM || v > MAX_DIM_CM)) {
    return undefined;
  }
  sides.sort((a, b) => b - a);
  return { l: sides[0]!, w: sides[1]!, h: sides[2]! };
}

/** The volume of the box the object would fit in. */
export function dimsLitres(d: Dims): number {
  return (d.l * d.w * d.h) / 1000;
}

type Sized = Pick<ItemDraft, "sizeBucket" | "quantity" | "dimensionsCm" | "dimensionsSource">;

/**
 * How much container this capture takes: its size when known, else its
 * bucket's litres from the server, times quantity. Undefined when neither is
 * known -- an older backend sent no litres -- and undefined never excludes.
 */
export function needLitres(item: Partial<Sized>, sizeLitres?: SizeLitres): number | undefined {
  const qty = Math.max(1, Math.floor(item.quantity ?? 1));
  const dims = normalizeDims(item.dimensionsCm);
  if (dims) return dimsLitres(dims) * qty;
  const each = sizeLitres?.[item.sizeBucket ?? "M"];
  return typeof each === "number" ? each * qty : undefined;
}

/** The item's size only when it was MEASURED, and so may rule a container out. */
export function measuredDims(item: Partial<Sized>): Dims | undefined {
  if (item.dimensionsSource !== "lidar" && item.dimensionsSource !== "manual") return undefined;
  return normalizeDims(item.dimensionsCm);
}

function capacityOf(b: Box): number | undefined {
  return typeof b.capacityL === "number" && b.capacityL > 0 ? b.capacityL : undefined;
}

/** The fill, when its source says it means something; else undefined. */
export function knownFill(b: Box): number | undefined {
  if (typeof b.fillPct !== "number") return undefined;
  switch (b.fillSource) {
    case "observed":
    case "lidar":
    case "estimated":
      return b.fillPct;
    default:
      return undefined;
  }
}

function fillIsFact(b: Box): boolean {
  return knownFill(b) !== undefined && (b.fillSource === "observed" || b.fillSource === "lidar");
}

/** Both capacity and fill are known, so filing into it can move the fill. */
export function tracksFill(b: Box): boolean {
  return capacityOf(b) !== undefined && knownFill(b) !== undefined;
}

function fits(item: Dims, inside: Dims): boolean {
  const slack = 1.05;
  return item.l <= inside.l * slack && item.w <= inside.w * slack && item.h <= inside.h * slack;
}

/**
 * Whether the container can take this, on what is KNOWN. Mirrors canHold in
 * engine.go: every refusal is a fact meeting a fact.
 */
export function canHold(b: Box, need: number | undefined, measured?: Dims): boolean {
  const capacity = capacityOf(b);
  if (capacity !== undefined && need !== undefined && need > capacity * 1.1) return false;
  const inside = normalizeDims(b.interiorCm);
  if (measured && inside && !fits(measured, inside)) return false;
  if (fillIsFact(b)) {
    const fill = knownFill(b)!;
    if (fill >= 100) return false;
    if (capacity !== undefined && need !== undefined && fill / 100 + need / capacity > 1.1) return false;
  }
  return true;
}

/**
 * A copy of the box with litres more in it, where its fill is known -- what
 * the next item from the same photo should see. An unknown fill stays unknown.
 */
export function withLitres(b: Box, litres: number | undefined): Box {
  const capacity = capacityOf(b);
  const fill = knownFill(b);
  if (capacity === undefined || fill === undefined || !litres || litres <= 0) return b;
  return { ...b, fillPct: fill + (litres / capacity) * 100 };
}

/**
 * The words beside a fill bar. Zero is a real fill and reads "Empty"; nobody
 * knowing reads "Not recorded". An estimate says it is one.
 */
export function fillLabel(b: Box): string {
  const fill = knownFill(b);
  if (fill === undefined) return "Not recorded";
  const pct = Math.round(fill);
  if (b.fillSource === "estimated") return pct <= 0 ? "Empty, estimated" : `~${pct}%, estimate`;
  if (pct <= 0) return "Empty";
  return b.fillSource === "lidar" ? `${pct}%, measured` : `${pct}% full`;
}

const THIRTY_DAYS_MS = 30 * 24 * 60 * 60 * 1000;

/**
 * Whether to ask "how full is it now?" when filing into this container: when
 * nobody knows, or when what is known is a sum of guesses that is getting
 * full or has not been checked in a month. Never for an area -- a garage has
 * no fill level -- and never asked of a fresh observation.
 */
export function wantsFillCheck(b: Box, now: number = Date.now()): boolean {
  if (b.isArea) return false;
  const fill = knownFill(b);
  if (fill === undefined) return true;
  if (b.fillSource !== "estimated") return false;
  if (fill >= 75) return true;
  const checked = b.fillCheckedAt ? Date.parse(b.fillCheckedAt) : NaN;
  return !Number.isFinite(checked) || now - checked > THIRTY_DAYS_MS;
}
