// Moving into and out of the demo: its own store, its own photo directory,
// its own inventory -- and on the way out, all three gone.

import { reloadForSwitch } from "../offline";
import { deleteDemoCaptures, useDemoCaptures } from "../photo";
import { clearDemoStore, useDemoStore } from "../storage";
import { beginDemo, demoWasOn, endDemo } from "./mode";

/** Starts the demo: from here on the queue, caches and photos are the demo's. */
export function enterDemo(): void {
  useDemoStore(true);
  useDemoCaptures(true);
  beginDemo();
  reloadForSwitch();
}

/**
 * Ends the demo and deletes everything it made. Back in the real store,
 * everything there is exactly as it was: the demo never wrote to it.
 */
export async function leaveDemo(): Promise<void> {
  endDemo();
  clearDemoStore();
  useDemoStore(false);
  useDemoCaptures(false);
  reloadForSwitch();
  await deleteDemoCaptures().catch(() => {
    // A leftover photo is only sample data, and the next demo starts clean anyway.
  });
}

/**
 * At launch, BEFORE anything reads the queue: back into the demo if it was
 * on when the app was closed. Returns whether it was.
 */
export function resumeDemoIfOn(): boolean {
  if (!demoWasOn()) return false;
  useDemoStore(true);
  useDemoCaptures(true);
  beginDemo();
  return true;
}
