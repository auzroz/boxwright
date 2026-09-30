import {
  blankDraft,
  draftFromIdentified,
  entriesFor,
  firstIncomplete,
  pendingSummary,
  reviewButtonLabel,
  seedChoices,
  topChoice,
} from "./draft";
import type { Destination, Draft } from "./draft";
import type { Box, ItemDraft, PendingCapture, Recommendation } from "./types";

function named(name: string, category = "tools"): Draft {
  const d = blankDraft();
  return { ...d, item: { ...d.item, name, category } };
}

function box(id: string): Box {
  return {
    id,
    name: `Box ${id}`,
    parentId: "p",
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
  };
}

describe("review", () => {
  test("the button names the next thing to do", () => {
    expect(reviewButtonLabel([])).toBe("Add an item");
    expect(reviewButtonLabel([named("")])).toBe("Name it first");
    expect(reviewButtonLabel([named("Drill", "")])).toBe("Pick a category");
    expect(reviewButtonLabel([named("Drill")])).toBe("Find a box");
    expect(reviewButtonLabel([named("Drill"), named("Jar", "")])).toBe("Pick a category for “Jar”");
    expect(reviewButtonLabel([named("Drill"), named("Jar")])).toBe("Find boxes for 2 items");
  });

  test("the first incomplete item is found by position", () => {
    const list = [named("Drill"), named(""), named("Jar", "")];
    expect(firstIncomplete(list)?.index).toBe(1);
  });

  test("a model's quantity of zero is floored to one", () => {
    const d = draftFromIdentified({ ...blankDraft().item, name: "x", category: "Tools", quantity: 0 } as ItemDraft, []);
    expect(d.item.quantity).toBe(1);
    expect(d.proposed).toEqual({ key: "tools", label: "Tools", byModel: true });
  });
});

describe("destinations", () => {
  test("the top candidate is chosen, else the new container", () => {
    const rec: Recommendation = { candidates: [{ box: box("a"), score: 1, reasons: [] }] };
    expect(topChoice(rec, undefined)).toEqual({ kind: "box", boxId: "a", name: "Box a" });
    const fresh: Recommendation = {
      candidates: [],
      newContainer: { sizeBucket: "M", access: "normal", label: "Tools 1", reason: "nothing fits" },
    };
    expect(topChoice(fresh, undefined)?.kind).toBe("new");
    expect(topChoice(undefined, undefined)).toBeNull();
  });

  test("a choice made by hand survives a fresh recommendation", () => {
    const list = [named("Drill"), named("Jar")];
    const mine: Destination = { kind: "box", boxId: "z", name: "Box z" };
    const recs: Recommendation[] = [
      { candidates: [{ box: box("a"), score: 1, reasons: [] }] },
      { candidates: [{ box: box("b"), score: 1, reasons: [] }] },
    ];
    const seeded = seedChoices(list, recs, null, { [list[0]!.id]: mine, gone: mine });
    expect(seeded[list[0]!.id]).toBe(mine);
    expect(seeded[list[1]!.id]).toEqual({ kind: "box", boxId: "b", name: "Box b" });
    expect(seeded.gone).toBeUndefined();
  });

  test("entries carry keys fixed by position and wait for every destination", () => {
    const list = [named("Drill"), named("Jar")];
    const a: Destination = { kind: "box", boxId: "a", name: "Box a" };
    expect(entriesFor("cap", list, { [list[0]!.id]: a })).toBeNull();
    const entries = entriesFor("cap", list, { [list[0]!.id]: a, [list[1]!.id]: a });
    expect(entries?.map((e) => e.entryId)).toEqual(["cap-0", "cap-1"]);
    expect(entries?.[0]?.boxId).toBe("a");
  });
});

test("the pending summary lists only what is there", () => {
  const c = (status: PendingCapture["status"]): PendingCapture => ({
    captureId: status,
    queuedAt: 0,
    photoName: "",
    status,
    attempts: 0,
  });
  expect(pendingSummary([c("ready"), c("ready"), c("identifying")])).toBe("2 ready to review · 1 still working");
  expect(pendingSummary([c("failed")])).toBe("1 failed");
});
