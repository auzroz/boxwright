// The category vocabulary, as the app sees it.
//
// The list below is a SEED, not the authority. Category is stored as a Homebox
// TAG and a box's affinity is counted from the tags of what it holds, so the
// user's own tags ARE the vocabulary. Measured against the real inventory this
// was built for, 11 of 13 tags -- Appliances, IOT, Servers, Important, General,
// Health, Micro Masterpieces, MM 2023-2026 -- are absent from this list, and the
// very first item filed used "Health". A picker closed over these eleven keys
// would have made those categories uncataloguable and split their box affinity.
//
// So GET /api/v1/categories is the source: the live tags first, then whichever
// of these seeds are still unused. What remains here serves two narrower jobs --
// the offline fallback for a phone that has never reached the backend, and
// turning a key back into something readable.

import type { CategoryOption } from "./types";

/**
 * The seed vocabulary: what an empty Homebox gets offered before it has any
 * tags of its own. Mirrors the ordered canonical list in
 * backend/internal/placement/categories.go.
 */
const CANONICAL_LABELS: Record<string, string> = {
  tools: "Tools",
  electronics: "Electronics",
  kitchen: "Kitchen",
  clothing: "Clothing",
  "books-media": "Books & media",
  decor: "Decor",
  "seasonal-holiday": "Seasonal & holiday",
  "sports-outdoor": "Sports & outdoor",
  documents: "Documents",
  "toys-games": "Toys & games",
  other: "Other",
};

/**
 * What the picker offers when the category list has never been fetched and
 * nothing is cached -- a first run in a storage unit with no signal.
 *
 * Ordered the way the contract orders unused seeds (alphabetically), so the
 * fallback and a real answer for an empty Homebox look the same rather than
 * reshuffling under the user's thumb the moment a fetch lands.
 */
export const FALLBACK_CATEGORIES: readonly CategoryOption[] = Object.entries(CANONICAL_LABELS)
  .map(([key, label]) => ({ key, label, inUse: false, itemCount: 0, canonical: true }))
  .sort((a, b) => a.label.localeCompare(b.label));

/**
 * Mirrors placement.NormalizeCategory in Go: trim, lowercase, spaces to
 * hyphens, empty to "other". Shape only -- it never substitutes one category
 * for another. Kept identical because this is the key that gets written, and a
 * key the engine cannot compare scores zero against every box.
 */
export function normalizeCategory(raw: string): string {
  const s = raw.trim().toLowerCase().replace(/ /g, "-");
  return s === "" ? "other" : s;
}

/**
 * The key for a category somebody wrote -- the user, or a vision model -- and
 * "" when they wrote nothing.
 *
 * Deliberately unlike normalizeCategory at the empty string: a missing category
 * is a question to put to the user, not a licence to tag their item "Other".
 * Everything non-empty passes through unchanged in meaning, including terms we
 * have never seen; folding those onto our own list is the exact mistake this
 * module was rewritten to stop making.
 */
export function categoryKeyFor(raw: string): string {
  return raw.trim() === "" ? "" : normalizeCategory(raw);
}

/** "micro-masterpieces" -> "Micro masterpieces". Last resort for a bare key. */
function humanizeCategoryKey(key: string): string {
  const words = key.split("-").filter((w) => w !== "");
  if (words.length === 0) return key;
  return words.map((w, i) => (i === 0 ? w.charAt(0).toUpperCase() + w.slice(1) : w)).join(" ");
}

/**
 * Human label for a key. Prefers the label the server sent (which is the user's
 * own tag, spelled their way), then the seed list, then the key itself made
 * readable. A key is never hidden for being unrecognised.
 */
export function categoryLabel(key: string, options?: readonly CategoryOption[]): string {
  const found = options?.find((o) => o.key === key);
  if (found && found.label !== "") return found.label;
  return CANONICAL_LABELS[key] ?? humanizeCategoryKey(key);
}

/** True when `key` is already part of the offered vocabulary. */
export function isKnownCategory(options: readonly CategoryOption[], key: string): boolean {
  return options.some((o) => o.key === key);
}

/**
 * Reads a category list off the wire or off disk.
 *
 * Both sources are untrusted enough to be worth a pass: a duplicate key would
 * render two chips for one category and collide as a React key, and a blank one
 * would be a tappable chip that selects nothing. Order is preserved exactly --
 * the server decides it (in-use by descending count, then unused seeds), and
 * re-sorting here would quietly undo that.
 */
export function sanitizeCategories(raw: unknown): CategoryOption[] {
  if (!Array.isArray(raw)) return [];
  const seen = new Set<string>();
  const out: CategoryOption[] = [];
  for (const entry of raw) {
    if (typeof entry !== "object" || entry === null) continue;
    const c = entry as Partial<CategoryOption>;
    const key = typeof c.key === "string" ? c.key.trim() : "";
    if (key === "" || seen.has(key)) continue;
    seen.add(key);
    const label = typeof c.label === "string" ? c.label.trim() : "";
    out.push({
      key,
      label: label === "" ? categoryLabel(key) : label,
      inUse: c.inUse === true,
      itemCount: typeof c.itemCount === "number" && Number.isFinite(c.itemCount) ? c.itemCount : 0,
      canonical: c.canonical === true,
    });
  }
  return out;
}

/**
 * The chips to draw: the offered vocabulary, plus the current selection when it
 * is not part of it.
 *
 * A category the list does not contain is the normal outcome of the model
 * proposing one, and it goes FIRST so that the thing about to create a new tag
 * is the thing under the user's eyes -- not somewhere down a wrapped list of
 * twenty chips where it can be confirmed by never being looked at.
 */
export function pickerOptions(
  options: readonly CategoryOption[],
  selectedKey: string,
  selectedLabel = "",
): CategoryOption[] {
  if (selectedKey === "" || isKnownCategory(options, selectedKey)) return [...options];
  const label = selectedLabel.trim();
  return [
    {
      key: selectedKey,
      label: label === "" ? categoryLabel(selectedKey) : label,
      inUse: false,
      itemCount: 0,
      canonical: false,
    },
    ...options,
  ];
}

/**
 * Resolves free text the user typed (or a model proposed) against the offered
 * vocabulary, returning the existing option when it names one.
 *
 * Shape-folding alone is not enough, because the chips render LABELS while the
 * wire carries KEYS, and the two differ wherever a label has punctuation the
 * fold does not: the seed "Books & Media" has the key "books-media". Someone
 * who reads a chip and types what it says would otherwise propose
 * "books-&-media" as a brand new category and get a duplicate tag in Homebox
 * beside the one they were looking at.
 */
export function resolveTypedCategory(
  text: string,
  options: CategoryOption[],
): CategoryOption | null {
  const trimmed = text.trim();
  if (trimmed === "") return null;
  const key = categoryKeyFor(trimmed);
  const label = trimmed.toLowerCase();
  const loose = matchKey(trimmed);
  return (
    options.find((o) => o.key === key) ??
    options.find((o) => o.label.trim().toLowerCase() === label) ??
    // Through the same fold, which catches a key typed against an option whose
    // label is spelled differently.
    options.find((o) => categoryKeyFor(o.label) === key) ??
    // Last resort, and the reason matchKey exists: "Books and Media" has to
    // find "Books & Media". The seed's label carries an ampersand its key does
    // not, so somebody reading the chip and typing what it says would
    // otherwise propose a brand new category and get a duplicate tag in
    // Homebox beside the one they were looking at.
    options.find((o) => matchKey(o.label) === loose || matchKey(o.key) === loose) ??
    null
  );
}

/**
 * A fold for COMPARING two spellings of a category. Never for writing one.
 *
 * The distinction is the same one the backend draws between MatchCategory and
 * NormalizeCategory, and for the same reason: this one deliberately loses
 * information -- "&" becomes "and", punctuation and separators go -- which
 * makes it good at recognising that two names mean one thing and completely
 * unfit for deciding what to call a new tag. Everything that reaches Homebox
 * goes through categoryKeyFor, which only reshapes.
 */
function matchKey(raw: string): string {
  return raw
    .trim()
    .toLowerCase()
    .replace(/&/g, " and ")
    .replace(/[^a-z0-9]+/g, "");
}
