import {
  categoryKeyFor,
  categoryLabel,
  normalizeCategory,
  pickerOptions,
  resolveTypedCategory,
  sanitizeCategories,
} from "./categories";
import type { CategoryOption } from "./types";

const option = (key: string, label: string, canonical = false): CategoryOption => ({
  key,
  label,
  inUse: false,
  itemCount: 0,
  canonical,
});

// The vocabulary is the USER'S. Folding an unfamiliar category onto our seed
// list is the mistake this module was rewritten to stop making.
describe("the fold does not rewrite the user's vocabulary", () => {
  test("an unfamiliar category keeps its meaning", () => {
    expect(normalizeCategory("Micro Masterpieces")).toBe("micro-masterpieces");
    expect(normalizeCategory("Health")).toBe("health");
  });

  test("only the shape changes: no aliasing, no mapping onto our seeds", () => {
    // "Books" must NOT become "books-media". A write path that aliases creates
    // a second tag in Homebox beside the one the user already had.
    expect(normalizeCategory("Books")).toBe("books");
    expect(normalizeCategory("Power Tools")).toBe("power-tools");
  });

  test("empty is 'other' when normalizing but NOT when reading what someone wrote", () => {
    // A missing category is a question to put to the user, not a licence to
    // tag their item "Other".
    expect(normalizeCategory("   ")).toBe("other");
    expect(categoryKeyFor("   ")).toBe("");
  });
});

describe("keys and labels", () => {
  test("the server's label wins, because it is the user's own spelling", () => {
    expect(categoryLabel("micro-masterpieces", [option("micro-masterpieces", "Micro Masterpieces")]))
      .toBe("Micro Masterpieces");
  });

  test("an unknown key is made readable rather than hidden", () => {
    expect(categoryLabel("micro-masterpieces")).toBe("Micro masterpieces");
  });

  test("a blank label falls back instead of rendering an empty chip", () => {
    expect(categoryLabel("tools", [option("tools", "")])).toBe("Tools");
  });
});

// The chips render LABELS while the wire carries KEYS, and the two differ
// wherever a label has punctuation the fold does not.
describe("typing what a chip says finds that chip", () => {
  const options = [option("books-media", "Books & Media", true), option("tools", "Tools", true)];

  test("by key", () => {
    expect(resolveTypedCategory("Tools", options)?.key).toBe("tools");
  });

  test("by the label the user can actually see", () => {
    expect(resolveTypedCategory("books & media", options)?.key).toBe("books-media");
  });

  test("through the fold, so 'Books and Media' is not a new category", () => {
    expect(resolveTypedCategory("Books and Media", options)?.key).toBe("books-media");
  });

  test("something genuinely new resolves to nothing, which is a legitimate answer", () => {
    expect(resolveTypedCategory("Micro Masterpieces", options)).toBeNull();
  });

  // The loose fold is for COMPARING, never for writing. What comes back is
  // always an option that already exists, so nothing it matches can reach
  // Homebox as a new tag name.
  test("matching loosely never invents a name", () => {
    const found = resolveTypedCategory("BOOKS & MEDIA!!", options);
    expect(found).not.toBeNull();
    expect(options).toContain(found);
  });
});

describe("the picker", () => {
  const options = [option("tools", "Tools", true)];

  test("shows a proposed category that is not in the vocabulary yet", () => {
    const shown = pickerOptions(options, "micro-masterpieces", "Micro Masterpieces");
    expect(shown[0]).toMatchObject({ key: "micro-masterpieces", label: "Micro Masterpieces", canonical: false });
    expect(shown).toHaveLength(2);
  });

  test("does not duplicate one that is", () => {
    expect(pickerOptions(options, "tools")).toHaveLength(1);
  });
});

describe("a category list off the wire or off disk", () => {
  test("drops duplicates and blanks, which would collide as React keys", () => {
    const got = sanitizeCategories([
      { key: "tools", label: "Tools" },
      { key: "tools", label: "Tools again" },
      { key: "", label: "Nothing" },
    ]);
    expect(got.map((c) => c.key)).toEqual(["tools"]);
  });

  test("preserves the server's order, which encodes in-use first", () => {
    const got = sanitizeCategories([
      { key: "health", label: "Health" },
      { key: "tools", label: "Tools" },
    ]);
    expect(got.map((c) => c.key)).toEqual(["health", "tools"]);
  });

  test("anything that is not a list is an empty list, never a crash", () => {
    expect(sanitizeCategories(null)).toEqual([]);
    expect(sanitizeCategories({ categories: [] })).toEqual([]);
  });
});
