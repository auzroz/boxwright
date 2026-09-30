// First-run setup, as a list of steps and the rules for moving between them.
// Pure, so the rules are tested without a screen; SetupFlow.tsx draws them.
//
// Setup is for a fresh install. Somebody already using the app -- a saved
// connection and no record of setup -- has done all of this the long way and
// is never sent through it.

import type { Prefs } from "./prefs";

export type SetupStep = "welcome" | "connect" | "locations" | "sizes" | "prefs" | "ready";

export const SETUP_STEPS: readonly SetupStep[] = ["welcome", "connect", "locations", "sizes", "prefs", "ready"];

/** The numbered steps, for "Step 2 of 4". Welcome and Ready are not counted. */
const NUMBERED: readonly SetupStep[] = ["connect", "locations", "sizes", "prefs"];

export function isSetupStep(v: unknown): v is SetupStep {
  return typeof v === "string" && (SETUP_STEPS as readonly string[]).includes(v);
}

/**
 * Where to start: "done" to go straight to the app, else the step to show.
 *
 * `configured` is whether a server connection is saved. Past the connect
 * step, a relaunch resumes where it left off -- but never on a step that
 * needs a server when there is none (the Keychain can be wiped by a restore),
 * which goes back to connect.
 */
export function startingStep(p: Prefs, configured: boolean): SetupStep | "done" {
  if (p.setupDone === true) return "done";
  const saved = isSetupStep(p.setupStep) ? p.setupStep : undefined;
  if (saved === undefined) return configured ? "done" : "welcome";
  if (!configured && saved !== "welcome") return "connect";
  return saved;
}

export function nextStep(step: SetupStep): SetupStep | "done" {
  const i = SETUP_STEPS.indexOf(step);
  return SETUP_STEPS[i + 1] ?? "done";
}

export function previousStep(step: SetupStep): SetupStep | undefined {
  const i = SETUP_STEPS.indexOf(step);
  return i > 0 ? SETUP_STEPS[i - 1] : undefined;
}

/** "Step 2 of 4", or undefined for Welcome and Ready. */
export function stepLabel(step: SetupStep): { index: number; count: number } | undefined {
  const i = NUMBERED.indexOf(step);
  return i < 0 ? undefined : { index: i + 1, count: NUMBERED.length };
}
