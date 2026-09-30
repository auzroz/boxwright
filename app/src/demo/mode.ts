// Whether the app is in the demo, and the demo's inventory while it is.
//
// The demo is a separate world, not a flag on the real one: its captures,
// queue and caches live in their own MMKV instance and its photos in their
// own directory (storage.ts, photo.ts), so nothing taken in the demo can ever
// be sent to a real server -- not by a flush, not by identification resuming
// after a switch. Leaving deletes all of it. Only the preferences, which
// belong to the phone, are shared.

import { useSyncExternalStore } from "react";

import { prefs, setPrefs } from "../prefs";
import { DemoInventory } from "./backend";

let inventory: DemoInventory | null = null;
const listeners = new Set<() => void>();

function emit(): void {
  for (const l of listeners) l();
}

export function demoActive(): boolean {
  return inventory !== null;
}

/** The demo's inventory. Only meaningful while demoActive(). */
export function demoInventory(): DemoInventory {
  inventory ??= new DemoInventory();
  return inventory;
}

/** Whether the demo was left running when the app was last closed. */
export function demoWasOn(): boolean {
  return prefs().mode === "demo";
}

/** Starts (or resumes) the demo with a fresh sample inventory. */
export function beginDemo(): void {
  inventory = new DemoInventory();
  if (prefs().mode !== "demo") setPrefs({ mode: "demo" });
  emit();
}

export function endDemo(): void {
  inventory = null;
  if (prefs().mode !== undefined) setPrefs({ mode: undefined });
  emit();
}

export function useDemo(): boolean {
  return useSyncExternalStore(
    (l) => {
      listeners.add(l);
      return () => listeners.delete(l);
    },
    demoActive,
  );
}

/** For tests. */
export function resetDemoForTest(): void {
  inventory = null;
  listeners.clear();
}
