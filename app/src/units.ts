// How measurements are shown and typed: metric (cm, litres) or imperial
// (inches, US gallons).
//
// Display and input only. Everything stored and sent stays metric -- Homebox
// fields, the engine, LiDAR -- so switching is instant, lossless and changes
// nothing anywhere but this phone's screen. The choice is a per-device
// preference, not inventory data; it defaults from the phone's region.

import { prefs, resetPrefsForTest, setPrefs, usePrefs } from "./prefs";
import type { Dims } from "./types";

export type UnitSystem = "metric" | "imperial";

const CM_PER_INCH = 2.54;
const LITRES_PER_US_GALLON = 3.785411784;
const MIN_CM = 0.5;
const MAX_CM = 300;

/** The three regions that do not use metric day to day. */
const IMPERIAL_REGIONS = new Set(["US", "LR", "MM"]);

/** Metric unless the phone's region is one that measures in inches. */
export function defaultUnits(locale: string | undefined = systemLocale()): UnitSystem {
  const region = locale?.split(/[-_]/)[1]?.toUpperCase();
  return region && IMPERIAL_REGIONS.has(region) ? "imperial" : "metric";
}

function systemLocale(): string | undefined {
  try {
    return Intl.DateTimeFormat().resolvedOptions().locale;
  } catch {
    return undefined;
  }
}

/** The chosen system, or the region's default until someone chooses. */
export function units(): UnitSystem {
  return chosen(prefs().units);
}

function chosen(saved: unknown): UnitSystem {
  return saved === "metric" || saved === "imperial" ? saved : defaultUnits();
}

export function setUnits(next: UnitSystem): void {
  setPrefs({ units: next });
}

export function toggleUnits(): void {
  setUnits(units() === "metric" ? "imperial" : "metric");
}

/** The current system, re-rendering whatever uses it when it changes. */
export function useUnits(): UnitSystem {
  return chosen(usePrefs().units);
}

/** For tests: forget the cached choice so the next read goes to storage. */
export function resetUnitsForTest(): void {
  resetPrefsForTest();
}

// ---------------------------------------------------------------------------
// Showing
// ---------------------------------------------------------------------------

function trim(v: number, places: number): string {
  const f = 10 ** places;
  return String(Math.round(v * f) / f);
}

/** The unit a length is typed and shown in. */
export function lengthUnit(system: UnitSystem): "cm" | "in" {
  return system === "metric" ? "cm" : "in";
}

/** The unit a capacity is typed and shown in. */
export function volumeUnit(system: UnitSystem): "L" | "gal" {
  return system === "metric" ? "L" : "gal";
}

/** One length in the chosen system: whole-ish centimetres, or inches to a tenth. */
export function lengthValue(cm: number, system: UnitSystem): string {
  return system === "metric" ? trim(cm, 1) : trim(cm / CM_PER_INCH, 1);
}

/** "32 × 20 × 12 cm" or "12.6 × 7.9 × 4.7 in". */
export function formatDimsIn(d: Dims, system: UnitSystem): string {
  const v = (cm: number) => lengthValue(cm, system);
  return `${v(d.l)} × ${v(d.w)} × ${v(d.h)} ${lengthUnit(system)}`;
}

/** "30 x 20 x 10" in the chosen system, for a field someone will edit. */
export function dimsFieldText(d: Dims, system: UnitSystem): string {
  const v = (cm: number) => lengthValue(cm, system);
  return `${v(d.l)} x ${v(d.w)} x ${v(d.h)}`;
}

/** "102 L" or "27 gal". Gallons to a tenth: 18- and 27-gallon are how they are sold. */
export function formatVolume(litres: number, system: UnitSystem): string {
  return system === "metric" ? `${Math.round(litres)} L` : `${trim(litres / LITRES_PER_US_GALLON, 1)} gal`;
}

// ---------------------------------------------------------------------------
// Reading what someone typed
// ---------------------------------------------------------------------------

/**
 * A size as typed, in centimetres, longest side first. Three numbers joined by
 * x, ×, * or commas, in the chosen system unless a unit says otherwise:
 * "30 x 20 x 10", "12 x 8 x 4 in", '12" x 8" x 4"', "300 x 200 x 100 mm".
 * Anything that is not three believable sides is undefined, never a guess.
 */
export function parseDimsIn(text: string, system: UnitSystem): Dims | undefined {
  let s = text.trim().toLowerCase();
  let factor = system === "metric" ? 1 : CM_PER_INCH;
  const unit = /(cm|mm|in|inches|inch|"|″)\s*$/.exec(s);
  if (unit) {
    const u = unit[1];
    factor = u === "cm" ? 1 : u === "mm" ? 0.1 : CM_PER_INCH;
    s = s.slice(0, unit.index);
  }
  s = s.replace(/["″]/g, "").replace(/(cm|mm|inches|inch|in)/g, "").replace(/[×*,]/g, "x");
  const parts = s.split("x").map((p) => p.trim());
  if (parts.length !== 3 || parts.some((p) => p === "")) return undefined;
  const n = parts.map(Number);
  if (n.some((v) => !Number.isFinite(v))) return undefined;
  const cm = n.map((v) => v * factor).sort((a, b) => b - a);
  if (cm.some((v) => v < MIN_CM || v > MAX_CM)) return undefined;
  const round = (v: number) => Math.round(v * 10) / 10;
  return { l: round(cm[0]!), w: round(cm[1]!), h: round(cm[2]!) };
}

/**
 * A capacity as typed, in whole litres: "100", "100 L", "27 gal", "27
 * gallons". A bare number is in the chosen system -- litres, or US gallons.
 */
export function parseCapacityIn(text: string, system: UnitSystem): number | undefined {
  const m = /^\s*(\d+(?:\.\d+)?)\s*(l|litres?|liters?|gal|gallons?)?\s*$/i.exec(text);
  if (!m) return undefined;
  const n = Number(m[1]);
  const gallons = m[2] ? /^gal/i.test(m[2]) : system === "imperial";
  const whole = Math.round(gallons ? n * LITRES_PER_US_GALLON : n);
  return whole > 0 && whole <= 10000 ? whole : undefined;
}
