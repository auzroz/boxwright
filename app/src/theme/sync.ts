// Keeps the Homebox theme name current and turns it into this phone's accent.

import { status } from "../api";
import { prefs, setPrefs, usePrefs } from "../prefs";
import { accentForTheme } from "./homebox";
import { KRAFT } from "./tokens";
import type { Accent } from "./tokens";

/**
 * Asks the server which theme the user's Homebox is wearing and remembers it.
 *
 * Remembered, so the next launch starts in the right colours rather than
 * flashing amber until the server answers -- and a failure keeps the last
 * answer, because a storage unit with no signal is no reason to change colour.
 * An answer with no theme (it could not be read, or an older server) does
 * clear it: that is the server saying it does not know.
 */
export async function refreshHomeboxTheme(signal?: AbortSignal): Promise<void> {
  try {
    const res = await status(signal);
    const theme = typeof res.homeboxTheme === "string" ? res.homeboxTheme : "";
    if (prefs().homeboxTheme !== theme) setPrefs({ homeboxTheme: theme });
  } catch {
    // Keep what we had.
  }
}

/** Forgets the theme, for when the app is pointed at a different Homebox. */
export function forgetHomeboxTheme(): void {
  if (prefs().homeboxTheme) setPrefs({ homeboxTheme: "" });
}

/** The accent to draw with: the Homebox one when wanted and known, else Boxwright's own. */
export function accentFrom(p: { matchHomebox?: boolean; homeboxTheme?: string }): Accent {
  return p.matchHomebox === false ? KRAFT : accentForTheme(p.homeboxTheme);
}

export function useAccent(): Accent {
  return accentFrom(usePrefs());
}
