import { DemoInventory, demoItems } from "./backend";

const NOW = Date.parse("2026-09-30T12:00:00Z");
const fresh = () => new DemoInventory(() => NOW);

describe("the sample inventory", () => {
  test("shows every kind of fill a real one can have", () => {
    const sources = new Set(fresh().boxes().boxes.map((b) => b.fillSource));
    // observed, measured, estimated, and nobody-knows -- solid, hatched and dashed bars.
    expect([...sources].sort()).toEqual(["", "estimated", "lidar", "observed"]);
  });

  test("has an empty container, an area, and container types of the user's own", () => {
    const res = fresh().boxes();
    expect(res.boxes.some((b) => !b.isArea && b.itemCount === 0)).toBe(true);
    expect(res.boxes.some((b) => b.isArea)).toBe(true);
    expect(res.containerTypes?.map((t) => t.name)).toContain("27-gallon tote");
    expect(res.sizeLitres).toEqual({ S: 2, M: 8, L: 25, XL: 60 });
  });

  test("never ships a scheme: no 'row' or 'tote' model beyond one named type", () => {
    const names = fresh().locations().locations.map((l) => l.name.toLowerCase());
    expect(names.some((n) => n.includes("row"))).toBe(false);
  });
});

describe("the flow a reviewer taps through", () => {
  test("identify finds the same three things every time", () => {
    expect(fresh().identify().map((i) => i.name)).toEqual(["Cordless drill", "Mason jars", "String lights"]);
  });

  test("each item gets a place, like with like first", () => {
    const res = fresh().recommend(demoItems());
    expect(res.recommendations).toHaveLength(3);
    expect(res.recommendations[0]?.candidates[0]?.box.name).toBe("Tools");
    expect(res.recommendations[2]?.candidates[0]?.box.name).toBe("Holiday");
  });

  // An empty container beats buying one, as in the engine -- so the spare is
  // filled first.
  test("something with nowhere to go is offered a new container of the user's own type", () => {
    const inv = fresh();
    inv.setContainers({ ids: ["spare"], set: { fill: { pct: 100, source: "observed", at: new Date(NOW).toISOString() } } });
    const odd = { ...demoItems()[0]!, category: "electronics", dimensionsCm: { l: 60, w: 40, h: 30 } };
    const rec = inv.recommend([odd]).recommendations[0]!;
    expect(rec.newContainer?.containerType).toBe("27-gallon tote");
  });

  test("filing lands, grows the place, and a fill answer replaces the guess", () => {
    const inv = fresh();
    const [drill] = demoItems();
    const res = inv.catalog({
      entries: [{ item: drill!, boxId: "tools", fillAfter: { pct: 75, source: "observed", at: new Date(NOW).toISOString() } }],
    });
    expect(res.results[0]?.error).toBeUndefined();
    expect(res.results[0]?.entity?.name).toBe("Cordless drill");
    const tools = inv.boxes().boxes.find((b) => b.id === "tools")!;
    expect(tools.categories.tools).toBe(10);
    expect(tools.fillPct).toBe(75);
    expect(tools.fillSource).toBe("observed");
  });

  test("filing into a new container creates it where it can be chosen next time", () => {
    const inv = fresh();
    inv.catalog({
      entries: [
        { item: demoItems()[0]!, newContainer: { label: "Electronics 1", parentId: "unit", sizeBucket: "L", access: "easy" } },
      ],
    });
    expect(inv.boxes().boxes.map((b) => b.name)).toContain("Electronics 1");
  });

  test("container settings and location choices stick for the session", () => {
    const inv = fresh();
    inv.setContainers({ ids: ["holiday"], set: { containerType: "Banker’s box", capacityL: 32 } });
    expect(inv.boxes().boxes.find((b) => b.id === "holiday")?.capacityL).toBe(32);
    inv.setLocations([{ id: "holiday", eligible: false }]);
    expect(inv.boxes().boxes.some((b) => b.id === "holiday")).toBe(false);
  });

  test("each demo starts from the same sample", () => {
    const a = fresh();
    a.setLocations([{ id: "tools", eligible: false }]);
    expect(fresh().boxes().boxes.some((b) => b.id === "tools")).toBe(true);
  });
});
