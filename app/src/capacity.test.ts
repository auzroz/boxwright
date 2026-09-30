import {
  canHold,
  fillSummary,
  measuredDims,
  needLitres,
  normalizeDims,
  wantsFillCheck,
  withLitres,
} from "./capacity";
import type { Box } from "./types";

const LITRES = { S: 2, M: 8, L: 25, XL: 60 };

function box(over: Partial<Box>): Box {
  return {
    id: "b",
    name: "Box",
    parentId: "",
    area: "",
    access: "",
    heavySafe: null,
    fragileSafe: null,
    capacityUnits: 0,
    usedUnits: 0,
    categories: {},
    gridX: 0,
    gridY: 0,
    isArea: false,
    ...over,
  };
}

const tote = (over: Partial<Box> = {}) =>
  box({ capacityL: 100, interiorCm: { l: 70, w: 45, h: 38 }, containerType: "27-gallon", ...over });

describe("sizes", () => {
  test("longest side first, and nonsense is no size at all", () => {
    expect(normalizeDims({ l: 10, w: 30, h: 20 })).toEqual({ l: 30, w: 20, h: 10 });
    expect(normalizeDims({ l: 0, w: 30, h: 20 })).toBeUndefined();
    expect(normalizeDims({ l: 1200, w: 30, h: 20 })).toBeUndefined();
    expect(normalizeDims(null)).toBeUndefined();
  });


  test("need is a size's volume, else the server's litres, times quantity", () => {
    expect(needLitres({ sizeBucket: "L" }, LITRES)).toBe(25);
    expect(needLitres({ sizeBucket: "S", quantity: 6 }, LITRES)).toBe(12);
    expect(needLitres({ sizeBucket: "XL", dimensionsCm: { l: 30, w: 20, h: 10 } }, LITRES)).toBe(6);
    // No litres from the server and no size: unknown, never a guess.
    expect(needLitres({ sizeBucket: "L" }, undefined)).toBeUndefined();
  });

  test("only a measured size is a fact", () => {
    const dims = { l: 30, w: 20, h: 10 };
    expect(measuredDims({ dimensionsCm: dims, dimensionsSource: "lidar" })).toEqual(dims);
    expect(measuredDims({ dimensionsCm: dims, dimensionsSource: "manual" })).toEqual(dims);
    expect(measuredDims({ dimensionsCm: dims, dimensionsSource: "vision" })).toBeUndefined();
  });
});

describe("canHold refuses on facts only, as the engine does", () => {
  test("nothing known refuses nothing", () => {
    expect(canHold(box({}), 500)).toBe(true);
    expect(canHold(tote(), undefined)).toBe(true);
  });

  test("bigger than the whole container", () => {
    expect(canHold(tote(), 120)).toBe(false);
    expect(canHold(tote(), 105)).toBe(true); // within the 10% slack
  });

  test("a measured item through a recorded opening", () => {
    expect(canHold(tote(), 10, { l: 90, w: 12, h: 12 })).toBe(false);
    expect(canHold(tote(), 10, { l: 65, w: 40, h: 10 })).toBe(true);
  });

  test("an observed fill excludes; an estimated one never does", () => {
    expect(canHold(tote({ fillPct: 100, fillSource: "observed" }), 1)).toBe(false);
    expect(canHold(tote({ fillPct: 80, fillSource: "lidar" }), 50)).toBe(false);
    expect(canHold(tote({ fillPct: 80, fillSource: "lidar" }), 25)).toBe(true);
    expect(canHold(tote({ fillPct: 150, fillSource: "estimated" }), 50)).toBe(true);
    // A fill with no source is unknown.
    expect(canHold(tote({ fillPct: 100, fillSource: "" }), 50)).toBe(true);
  });

  test("the next item sees what the last took, only where the fill is known", () => {
    expect(withLitres(tote({ fillPct: 50, fillSource: "observed" }), 25).fillPct).toBe(75);
    expect(withLitres(tote(), 25).fillPct).toBeUndefined();
  });
});

describe("what the screens say about fill", () => {
  test("a summary that says how it is known", () => {
    expect(fillSummary(tote({ fillPct: 40.4, fillSource: "estimated" }))).toBe("about 40% full (estimated)");
    expect(fillSummary(tote({ fillPct: 60, fillSource: "lidar" }))).toBe("about 60% full (measured)");
    expect(fillSummary(tote({ fillPct: 10, fillSource: "observed" }))).toBe("about 10% full");
    expect(fillSummary(tote())).toBeUndefined();
  });

  test("ask how full it is when nobody knows, or a guess is getting full or stale", () => {
    const now = Date.parse("2026-09-28T12:00:00Z");
    expect(wantsFillCheck(tote(), now)).toBe(true);
    expect(wantsFillCheck(tote({ fillPct: 40, fillSource: "observed", fillCheckedAt: "2026-01-01T00:00:00Z" }), now)).toBe(false);
    expect(wantsFillCheck(tote({ fillPct: 40, fillSource: "estimated", fillCheckedAt: "2026-09-27T00:00:00Z" }), now)).toBe(false);
    expect(wantsFillCheck(tote({ fillPct: 80, fillSource: "estimated", fillCheckedAt: "2026-09-27T00:00:00Z" }), now)).toBe(true);
    expect(wantsFillCheck(tote({ fillPct: 40, fillSource: "estimated", fillCheckedAt: "2026-08-01T00:00:00Z" }), now)).toBe(true);
    expect(wantsFillCheck(box({ isArea: true }), now)).toBe(false);
  });
});

