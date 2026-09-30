// This phone's own preferences: how measurements read, whether to wear the
// Homebox colours, how far first-run setup got. Per-device settings, not
// inventory data, so they live in MMKV beside the queue and never go to the
// server.
//
// One key, merged on every write. Each setting used to own the whole object,
// so saving one would have erased the others.

import { useSyncExternalStore } from "react";

import { readText, writeJson } from "./storage";

export const PREFS_KEY = "prefs.v1";

export interface Prefs {
  units?: "metric" | "imperial";
  /** Take the accent from the user's Homebox theme. On unless turned off. */
  matchHomebox?: boolean;
  /** The last theme name /status reported; kept so launch is not grey until it answers. */
  homeboxTheme?: string;
  /** First-run setup: where it got to, and whether it is over. See setup.ts. */
  setupStep?: string;
  setupDone?: boolean;
}

let cached: Prefs | undefined;
const listeners = new Set<() => void>();

export function prefs(): Prefs {
  if (cached === undefined) {
    try {
      const raw = readText(PREFS_KEY);
      const parsed: unknown = raw ? JSON.parse(raw) : {};
      cached = parsed && typeof parsed === "object" ? (parsed as Prefs) : {};
    } catch {
      cached = {};
    }
  }
  return cached;
}

/** Sets some preferences, keeping the rest. */
export function setPrefs(patch: Partial<Prefs>): void {
  const next = { ...prefs(), ...patch };
  cached = next;
  writeJson(PREFS_KEY, { version: 1, ...next });
  for (const l of listeners) l();
}

export function usePrefs(): Prefs {
  return useSyncExternalStore((l) => {
    listeners.add(l);
    return () => listeners.delete(l);
  }, prefs);
}

/** For tests: forget what was read so the next read goes to storage. */
export function resetPrefsForTest(): void {
  cached = undefined;
  listeners.clear();
}
