import { HOMEBOX_PRIMARY, MIN_TEXT_CONTRAST, MIN_UI_CONTRAST, accentFor, accentForTheme, contrast } from "./homebox";
import { KRAFT, palette } from "./tokens";

describe("accent from the Homebox theme", () => {
  // Its sage is just short of AA behind white text (4.3:1), so it is taken a
  // shade darker; still unmistakably the green.
  test("Homebox's own theme gives its sage", () => {
    const { accent } = accentForTheme("homebox");
    expect(accent).not.toBe(KRAFT.accent);
    const n = parseInt(accent.slice(1), 16);
    expect((n >> 8) & 255).toBeGreaterThan((n >> 16) & 255);
    expect(contrast("#5C7F67", accent)).toBeLessThan(1.2);
  });

  test("no theme, or one we do not know, keeps Boxwright's own", () => {
    expect(accentForTheme(undefined)).toEqual(KRAFT);
    expect(accentForTheme("")).toEqual(KRAFT);
    expect(accentForTheme("dark")).toEqual(KRAFT);
    expect(accentForTheme("something-new")).toEqual(KRAFT);
  });

  test("the name is matched loosely", () => {
    expect(accentForTheme(" Forest ")).toEqual(accentForTheme("forest"));
  });

  // Every one, because a light primary (aqua, pastel, a white one) behind
  // white button text is exactly what the guard exists for.
  test.each(Object.keys(HOMEBOX_PRIMARY))("%s: white text on it, and it on the paper, both pass", (name) => {
    const { accent, soft } = accentForTheme(name);
    expect(contrast("#FFFFFF", accent)).toBeGreaterThanOrEqual(MIN_TEXT_CONTRAST);
    expect(contrast(palette.ground, accent)).toBeGreaterThanOrEqual(MIN_UI_CONTRAST);
    // The tint stays light enough for ink on it.
    expect(contrast(palette.ink, soft)).toBeGreaterThanOrEqual(MIN_TEXT_CONTRAST);
  });

  test("a readable colour is left alone; a light one keeps its hue", () => {
    expect(accentFor("#1C4F82").accent).toBe("#1C4F82");
    const aqua = accentFor("#09E9F1").accent;
    expect(aqua).not.toBe("#09E9F1");
    // Still a blue-green: more green and blue than red.
    const n = parseInt(aqua.slice(1), 16);
    expect((n >> 16) & 255).toBeLessThan(n & 255);
  });

  test("the contrast ratio is WCAG's", () => {
    expect(contrast("#FFFFFF", "#000000")).toBeCloseTo(21, 5);
    expect(contrast("#777777", "#777777")).toBeCloseTo(1, 5);
  });
});
