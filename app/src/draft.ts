// The capture being reviewed, as the screens hold it: drafts, where each is
// going, and the small pure helpers that turn those into words. No React, so
// every screen can share them and jest can reach them.

import { categoryKeyFor, isKnownCategory } from "./categories";
import type {
  Access,
  Box,
  CategoryOption,
  Dims,
  ItemDraft,
  NewContainerSuggestion,
  PendingCapture,
  QueuedEntry,
  Recommendation,
  SizeBucket,
} from "./types";

/** A category the offered vocabulary does not contain yet, and who wrote it. */
export type Proposed = { key: string; label: string; byModel: boolean };

/**
 * One thing recognised in the photo (or added by hand), with the editing state
 * that belongs to it alone.
 *
 * `id` exists because nothing about an item is a stable key: two drafts can
 * share a name, and a name is empty while it is being typed.
 */
export type Draft = { id: string; item: ItemDraft; proposed: Proposed | null };

/**
 * Where one item is going. A box that exists, or one about to be created.
 *
 * `name` is what the user is shown and what the queue records, so that a
 * pending capture can say where it is headed without a box list to look it up
 * in.
 */
export type Destination =
  | { kind: "box"; boxId: string; name: string }
  | {
      kind: "new";
      name: string;
      sizeBucket: SizeBucket;
      access: Access;
      parentId: string;
      /** One of the user's own container types, when the suggestion named one. */
      containerType?: string;
      capacityL?: number;
      interiorCm?: Dims;
    };

/** An item the server refused, kept so the user can be told which and why. */
export type Failure = { entry: QueuedEntry; error: string };

/**
 * A manual capture starts with NO category rather than "other".
 *
 * "other" as a default is a tag invented on the user's behalf every time
 * somebody taps past the field, and it lands in Homebox looking like a
 * deliberate answer while contributing nothing to any box's affinity. One tap
 * on a chip is cheap; an inventory seeded with "Other" is not.
 */
export const emptyDraft: ItemDraft = {
  name: "",
  category: "",
  sizeBucket: "M",
  fragile: false,
  bulky: false,
  weightClass: "medium",
  notes: "",
  confidence: 0,
  quantity: 1,
};

let draftSeq = 0;
function newDraftId(): string {
  draftSeq += 1;
  return `draft-${draftSeq}`;
}

export function blankDraft(): Draft {
  return { id: newDraftId(), item: { ...emptyDraft }, proposed: null };
}

/**
 * Takes an identified item as the model gave it, including a category we have
 * never seen.
 *
 * The model is shown the live vocabulary and told it may propose something
 * new, so a category outside the list is an answer, not an error. Only the
 * shape is normalised; the model's own wording is kept so the chip reads the
 * way it wrote it and the user is asked to confirm a tag before it is created,
 * rather than finding it afterwards in Homebox.
 */
export function draftFromIdentified(identified: ItemDraft, vocabulary: readonly CategoryOption[]): Draft {
  const key = categoryKeyFor(identified.category);
  // Quantity is shown in a stepper and multiplied into capacity, so a model
  // that answers 0 or omits it would put a "0 ×" on screen. Floor it here
  // rather than trusting every provider to have been floored server-side.
  const quantity = Number.isFinite(identified.quantity) ? Math.floor(identified.quantity) : 1;
  return {
    id: newDraftId(),
    item: { ...identified, category: key, quantity: Math.max(1, quantity) },
    proposed:
      key !== "" && !isKnownCategory(vocabulary, key)
        ? { key, label: identified.category.trim(), byModel: true }
        : null,
  };
}

/** The first item that cannot be filed yet, so the user is told which. */
export function firstIncomplete(drafts: readonly Draft[]): { draft: Draft; index: number } | null {
  for (let i = 0; i < drafts.length; i++) {
    const draft = drafts[i];
    if (!draft) continue;
    if (draft.item.name.trim() === "" || draft.item.category === "") return { draft, index: i };
  }
  return null;
}

/** What the review screen's button says: the next thing to do, by name. */
export function reviewButtonLabel(drafts: readonly Draft[]): string {
  if (drafts.length === 0) return "Add an item";
  const missing = firstIncomplete(drafts);
  if (!missing) return drafts.length === 1 ? "Find a box" : `Find boxes for ${drafts.length} items`;
  const needsName = missing.draft.item.name.trim() === "";
  if (drafts.length === 1) return needsName ? "Name it first" : "Pick a category";
  return needsName ? `Name item ${missing.index + 1}` : `Pick a category for “${missing.draft.item.name.trim()}”`;
}

/** Turns the access bucket into something readable in a sentence. */
export function accessPhrase(access: NewContainerSuggestion["access"]): string {
  switch (access) {
    case "easy":
      return "easy to get to";
    case "deep":
      return "out of the way is fine";
    default:
      return "convenient";
  }
}

export function ago(ts: number, now: number = Date.now()): string {
  const minutes = Math.max(0, Math.round((now - ts) / 60_000));
  if (minutes < 1) return "just now";
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.round(minutes / 60);
  return hours < 24 ? `${hours}h ago` : `${Math.round(hours / 24)}d ago`;
}

export function plural(n: number, one: string, many: string): string {
  return n === 1 ? one : many;
}

/** "2 ready to review · 3 still working" -- whichever halves are non-zero. */
export function pendingSummary(captures: readonly PendingCapture[]): string {
  const ready = captures.filter((c) => c.status === "ready").length;
  const failed = captures.filter((c) => c.status === "failed").length;
  const working = captures.length - ready - failed;
  const parts: string[] = [];
  if (ready > 0) parts.push(`${ready} ready to review`);
  if (working > 0) parts.push(`${working} still working`);
  if (failed > 0) parts.push(`${failed} failed`);
  return parts.join(" · ");
}

/** "3 × " when it matters, "" when it does not. */
export function quantityPrefix(item: ItemDraft | undefined): string {
  return (item?.quantity ?? 1) > 1 ? `${item?.quantity} × ` : "";
}

export function itemName(item: ItemDraft | undefined): string {
  return item?.name?.trim() || "Untitled";
}

/** The destination for an accepted new-container suggestion. */
export function newContainerDestination(suggestion: NewContainerSuggestion, rec?: Recommendation): Destination {
  return {
    kind: "new",
    name: suggestion.label,
    sizeBucket: suggestion.sizeBucket,
    access: suggestion.access,
    // Put the new container alongside the nearest existing candidate, which is
    // what a person would do. With no candidates it lands at the top level and
    // can be moved in Homebox.
    parentId: rec?.candidates?.[0]?.box.parentId ?? "",
    containerType: suggestion.containerType,
    capacityL: suggestion.capacityL,
    interiorCm: suggestion.interiorCm,
  };
}

/** The destination the user gets for free: the best answer for this item. */
export function topChoice(rec: Recommendation | undefined, picks: Box[] | undefined): Destination | null {
  const box = rec?.candidates?.[0]?.box ?? picks?.[0];
  if (box) return { kind: "box", boxId: box.id, name: box.name };
  return rec?.newContainer ? newContainerDestination(rec.newContainer, rec) : null;
}

export function sameDestination(a: Destination | undefined, b: Destination): boolean {
  if (!a || a.kind !== b.kind) return false;
  return a.kind === "box" && b.kind === "box" ? a.boxId === b.boxId : true;
}

/**
 * The destination for each item: the one the user chose, or the engine's
 * best answer for the ones they have not touched.
 *
 * `previous` is why this takes an argument at all. Going back to the item
 * list and forward again re-runs the recommendation, and seeding from
 * scratch would quietly throw away every destination the user had picked by
 * hand -- they would return to the screen, see the engine's suggestions
 * again, and have no way to know their choices had been reverted. An
 * explicit choice outranks a fresh suggestion.
 *
 * Choices for drafts that no longer exist are dropped, so removing an item
 * on the way back does not leave its destination behind.
 */
export function seedChoices(
  list: readonly Draft[],
  recommendations: Recommendation[] | null,
  picks: Box[][] | null,
  previous?: Record<string, Destination>,
): Record<string, Destination> {
  const seeded: Record<string, Destination> = {};
  list.forEach((d, i) => {
    const kept = previous?.[d.id];
    if (kept) {
      seeded[d.id] = kept;
      return;
    }
    const top = topChoice(recommendations?.[i], picks?.[i]);
    if (top) seeded[d.id] = top;
  });
  return seeded;
}

/**
 * The entries for this capture, each carrying the idempotency key it will
 * keep for the rest of its life -- or null while an item has no destination.
 *
 * The key is `<captureId>-<index>` and is computed HERE, once, then stored
 * with the entry -- in React state now and on the offline queue if it has to
 * wait. It is deterministic so that calling this twice for the same drafts
 * produces the same keys, and it is never recomputed at send time: a partial
 * flush re-sends a SUBSET of the entries, at which point index 0 is a
 * different item and a recomputed key would match it against one that
 * already landed. captureId is unique per capture, so the pair is unique.
 */
export function entriesFor(
  captureId: string,
  drafts: readonly Draft[],
  map: Record<string, Destination>,
): QueuedEntry[] | null {
  const entries: QueuedEntry[] = [];
  for (const [index, draft] of drafts.entries()) {
    const destination = map[draft.id];
    if (!destination) return null;
    entries.push({
      item: draft.item,
      entryId: `${captureId}-${index}`,
      boxId: destination.kind === "box" ? destination.boxId : undefined,
      newContainer:
        destination.kind === "new"
          ? {
              label: destination.name,
              parentId: destination.parentId,
              sizeBucket: destination.sizeBucket,
              access: destination.access,
              containerType: destination.containerType,
              capacityL: destination.capacityL,
              interiorCm: destination.interiorCm,
            }
          : undefined,
      boxName: destination.name,
    });
  }
  return entries;
}

export function photoMissWarning(misses: number, total: number, hadPhoto: boolean): string {
  if (!hadPhoto || misses === 0) return "";
  if (misses === total) return "The photo did not upload.";
  return `The photo did not upload for ${misses} of the ${total} items.`;
}

/** Plain words for a size bucket. */
export const SIZE_WORDS: Record<SizeBucket, string> = { S: "Small", M: "Medium", L: "Large", XL: "Extra large" };
