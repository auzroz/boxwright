// The demo: a sample inventory that lives in memory on the phone, answering
// every call the app makes with the same shapes a Boxwright server would
// (types.ts), so the screens cannot tell. It is what App Review sees, and
// what anyone without a server yet can try.
//
// Not the engine. The ranking below is a few lines, enough to make the
// screens show what they are for: a place that already holds the category,
// fill bars of every kind (observed, measured, estimated, unknown), an empty
// container, and the suggestion to start a new one. Nothing leaves the phone
// and nothing is kept: a fresh demo starts from the same sample every time.

import type {
  AdoptResponse,
  Box,
  BoxesResponse,
  CatalogRequest,
  CatalogResponse,
  CategoriesResponse,
  CategoryOption,
  ContainerType,
  EntityTypesResponse,
  ItemDraft,
  LocationChoice,
  LocationNode,
  LocationsResponse,
  LocationsWriteResponse,
  PutContainersRequest,
  PutContainersResponse,
  Recommendation,
  RecommendResponse,
  SizeBucket,
  StatusResponse,
} from "../types";

const SIZE_LITRES: Record<SizeBucket, number> = { S: 2, M: 8, L: 25, XL: 60 };

/** How long "identifying" takes, so the waiting screen is seen, briefly. */
export const DEMO_IDENTIFY_MS = 1500;

interface Place {
  id: string;
  name: string;
  parentId?: string;
  hasChildren?: boolean;
  eligible: boolean;
  box?: Partial<Box>;
}

const TOTE = { containerType: "27-gallon tote", capacityL: 102, interiorCm: { l: 66, w: 41, h: 33 } };

function daysAgo(n: number, now: number): string {
  return new Date(now - n * 864e5).toISOString();
}

function samplePlaces(now: number): Place[] {
  return [
    { id: "garage", name: "Garage", hasChildren: true, eligible: true },
    { id: "shelf-a", name: "Shelf A", parentId: "garage", hasChildren: true, eligible: false },
    { id: "tools", name: "Tools", parentId: "shelf-a", eligible: true,
      box: { ...TOTE, fillPct: 55, fillSource: "observed", fillCheckedAt: daysAgo(3, now), access: "easy", categories: { tools: 9 } } },
    { id: "kitchen", name: "Kitchen spares", parentId: "shelf-a", eligible: true,
      box: { ...TOTE, fillPct: 80, fillSource: "estimated", fillCheckedAt: daysAgo(40, now), access: "normal", fragileSafe: true, categories: { kitchen: 14 } } },
    { id: "holiday", name: "Holiday", parentId: "shelf-a", eligible: true, box: { access: "deep", categories: { decor: 22 } } },
    { id: "unit", name: "Storage unit 12", hasChildren: true, eligible: false },
    { id: "camping", name: "Camping", parentId: "unit", eligible: true,
      box: { ...TOTE, fillPct: 30, fillSource: "lidar", fillCheckedAt: daysAgo(1, now), access: "normal", categories: { outdoor: 6 } } },
    { id: "books", name: "Books", parentId: "unit", eligible: true,
      box: { containerType: "Banker’s box", capacityL: 32, fillPct: 95, fillSource: "observed", fillCheckedAt: daysAgo(2, now), access: "deep", categories: { "books-media": 31 } } },
    { id: "spare", name: "Spare 1", parentId: "unit", eligible: true,
      box: { ...TOTE, fillPct: 0, fillSource: "observed", fillCheckedAt: daysAgo(1, now), access: "easy", categories: {} } },
  ];
}

const CATEGORY_LABELS: [string, string][] = [
  ["tools", "Tools"],
  ["kitchen", "Kitchen"],
  ["decor", "Decor"],
  ["outdoor", "Outdoor"],
  ["books-media", "Books & Media"],
  ["electronics", "Electronics"],
  ["clothing", "Clothing"],
];

/** What "the model" finds in any photo: a shelf of three things. */
export function demoItems(): ItemDraft[] {
  return [
    { name: "Cordless drill", category: "tools", sizeBucket: "M", fragile: false, bulky: false, weightClass: "medium",
      notes: "DeWalt DCD771", confidence: 0.94, quantity: 1, dimensionsCm: { l: 25, w: 20, h: 8 }, dimensionsSource: "vision" },
    { name: "Mason jars", category: "kitchen", sizeBucket: "S", fragile: true, bulky: false, weightClass: "light",
      notes: "", confidence: 0.88, quantity: 6, dimensionsCm: { l: 14, w: 9, h: 9 }, dimensionsSource: "vision" },
    { name: "String lights", category: "decor", sizeBucket: "S", fragile: false, bulky: false, weightClass: "light",
      notes: "", confidence: 0.71, quantity: 1, dimensionsCm: null },
  ];
}

export class DemoInventory {
  private places: Place[];
  private created = 0;

  constructor(private readonly now: () => number = Date.now) {
    this.places = samplePlaces(now());
  }

  status(): StatusResponse {
    return {
      version: "demo",
      apiVersion: 1,
      identification: true,
      aiProvider: "demo",
      clientHomebox: false,
      homebox: { ok: true },
    };
  }

  identify(): ItemDraft[] {
    return demoItems();
  }

  private count(p: Place): number {
    return Object.values(p.box?.categories ?? {}).reduce((a, n) => a + n, 0);
  }

  private parentName(p: Place): string {
    return this.places.find((x) => x.id === p.parentId)?.name ?? "";
  }

  private boxOf(p: Place): Box {
    const b = p.box ?? {};
    const capacityL = b.capacityL ?? null;
    const known = b.fillSource ? (b.fillPct ?? 0) : null;
    const units = capacityL ? Math.round(capacityL / 12.5) : 0;
    return {
      id: p.id,
      name: p.name,
      parentId: p.parentId ?? "",
      area: this.parentName(p),
      access: b.access ?? "",
      heavySafe: b.heavySafe ?? null,
      fragileSafe: b.fragileSafe ?? null,
      containerType: b.containerType ?? "",
      capacityL,
      interiorCm: b.interiorCm ?? null,
      fillPct: known,
      fillSource: b.fillSource ?? "",
      fillCheckedAt: b.fillCheckedAt ?? "",
      capacityUnits: units,
      usedUnits: known !== null ? Math.round((known / 100) * units) : 0,
      itemCount: this.count(p),
      categories: { ...(b.categories ?? {}) },
      gridX: 0,
      gridY: 0,
      isArea: p.hasChildren === true,
    };
  }

  private chosen(): Box[] {
    return this.places.filter((p) => p.eligible).map((p) => this.boxOf(p));
  }

  private containerTypes(): ContainerType[] {
    const seen = new Map<string, ContainerType>();
    for (const b of this.chosen()) {
      if (b.isArea || !b.containerType || !b.capacityL) continue;
      const t = seen.get(b.containerType) ?? { name: b.containerType, capacityL: b.capacityL, interiorCm: b.interiorCm ?? null, count: 0 };
      t.count += 1;
      seen.set(b.containerType, t);
    }
    return [...seen.values()].sort((a, b) => a.capacityL - b.capacityL);
  }

  boxes(): BoxesResponse {
    return { boxes: this.chosen(), stale: false, containerTypes: this.containerTypes(), sizeLitres: { ...SIZE_LITRES } };
  }

  categories(): CategoriesResponse {
    const counts: Record<string, number> = {};
    for (const p of this.places) for (const [k, n] of Object.entries(p.box?.categories ?? {})) counts[k] = (counts[k] ?? 0) + n;
    const categories: CategoryOption[] = CATEGORY_LABELS.map(([key, label]) => ({
      key,
      label,
      inUse: (counts[key] ?? 0) > 0,
      itemCount: counts[key] ?? 0,
      canonical: true,
    })).sort((a, b) => b.itemCount - a.itemCount);
    return { categories };
  }

  locations(): LocationsResponse {
    const depth = (p: Place): number => {
      const parent = this.places.find((x) => x.id === p.parentId);
      return parent ? depth(parent) + 1 : 0;
    };
    const locations: LocationNode[] = this.places.map((p) => ({
      id: p.id,
      name: p.name,
      parentId: p.parentId,
      parentName: this.parentName(p) || undefined,
      path: [this.parentName(p), p.name].filter((s) => s !== "").join(" > "),
      depth: depth(p),
      itemCount: this.count(p),
      hasChildren: p.hasChildren === true,
      eligible: p.eligible,
    }));
    return { locations, eligibleCount: locations.filter((l) => l.eligible).length };
  }

  setLocations(choices: LocationChoice[]): LocationsWriteResponse {
    return {
      results: choices.map((c) => {
        const p = this.places.find((x) => x.id === c.id);
        if (!p) return { id: c.id, eligible: c.eligible, error: "no such location" };
        p.eligible = c.eligible;
        return { id: c.id, eligible: c.eligible };
      }),
    };
  }

  adoptLocations(): AdoptResponse {
    return { scanned: this.places.length, marked: 0, adopted: [] };
  }

  entityTypes(): EntityTypesResponse {
    return { entityTypes: [{ id: "loc", name: "Location", isLocation: true }, { id: "item", name: "Item", isLocation: false }] };
  }

  setContainers(req: PutContainersRequest): PutContainersResponse {
    return {
      results: req.ids.map((id) => {
        const p = this.places.find((x) => x.id === id);
        if (!p) return { id, error: "no such container" };
        const b = { ...(p.box ?? {}) };
        const s = req.set;
        if (s.containerType !== undefined) b.containerType = s.containerType;
        if (s.capacityL !== undefined) b.capacityL = s.capacityL;
        if (s.interiorCm !== undefined) b.interiorCm = s.interiorCm;
        if (s.access !== undefined) b.access = s.access;
        if (s.heavySafe !== undefined) b.heavySafe = s.heavySafe;
        if (s.fragileSafe !== undefined) b.fragileSafe = s.fragileSafe;
        if (s.fill) {
          b.fillPct = s.fill.pct;
          b.fillSource = s.fill.source;
          b.fillCheckedAt = s.fill.at;
        }
        p.box = b;
        return { id, box: this.boxOf(p) };
      }),
    };
  }

  recommend(items: ItemDraft[]): RecommendResponse {
    // Scored together, as the server does: the second item sees what the first took.
    const taken: Record<string, number> = {};
    const recommendations = items.map((item) => {
      const rec = this.recommendOne(item, taken);
      const top = rec.candidates[0]?.box;
      if (top) taken[top.id] = (taken[top.id] ?? 0) + need(item);
      return rec;
    });
    return { recommendations, stale: false };
  }

  private recommendOne(item: ItemDraft, taken: Record<string, number>): Recommendation {
    const litres = need(item);
    const candidates = this.chosen()
      .filter((b) => b.isArea === item.bulky)
      .filter((b) => !(b.fillSource === "observed" && (b.fillPct ?? 0) >= 100))
      .map((b) => {
        const total = Object.values(b.categories).reduce((a, n) => a + n, 0);
        const share = total ? (b.categories[item.category] ?? 0) / total : 0;
        const reasons: string[] = [];
        let score = 5 * share;
        if (share > 0) reasons.push(`already holds ${item.category.replace(/-/g, " ")}`);
        if (b.capacityL && b.fillPct !== null && b.fillPct !== undefined) {
          const projected = b.fillPct / 100 + ((taken[b.id] ?? 0) + litres) / b.capacityL;
          score += 1.5 * Math.max(0, 1 - projected);
          if (projected > 1) {
            score -= 2.5;
            reasons.push("may not have room");
          }
        } else if (!b.isArea) {
          score += 0.75;
        }
        if ((b.itemCount ?? 0) === 0 && !b.isArea) {
          score = Math.max(score + 1.75, 2.75);
          reasons.push("empty, so it can start a group");
        }
        if (b.isArea) reasons.push("somewhere large things can stand");
        return { box: b, score, reasons };
      })
      .sort((a, b) => b.score - a.score)
      .slice(0, 5);
    const rec: Recommendation = { candidates };
    if (item.bulky) return candidates.length ? rec : { candidates, noPlace: { reason: "There is nowhere in the sample to stand it." } };
    if (!candidates.length || candidates[0]!.score < 2.75) {
      const types = this.containerTypes();
      const biggest = types[types.length - 1];
      rec.newContainer = {
        sizeBucket: "L",
        access: "easy",
        label: `${CATEGORY_LABELS.find(([k]) => k === item.category)?.[1] ?? "New"} 1`,
        reason: "no existing container is a good fit",
        ...(biggest ? { containerType: biggest.name, capacityL: biggest.capacityL, interiorCm: biggest.interiorCm ?? undefined } : {}),
      };
    }
    return rec;
  }

  catalog(req: CatalogRequest): CatalogResponse {
    const litres: Record<string, number> = {};
    const results = req.entries.map((entry) => {
      let target = entry.boxId ? this.places.find((p) => p.id === entry.boxId) : undefined;
      if (entry.newContainer) {
        this.created += 1;
        const nc = entry.newContainer;
        target = {
          id: `new-${this.created}`,
          name: nc.label,
          parentId: nc.parentId || undefined,
          eligible: true,
          box: {
            containerType: nc.containerType ?? "",
            capacityL: nc.capacityL,
            interiorCm: nc.interiorCm,
            access: nc.access,
            categories: {},
          },
        };
        this.places.push(target);
      }
      if (!target) return { fieldsWritten: false, photoUploaded: false, error: "that place is not in the sample" };
      const box = (target.box ??= { categories: {} });
      const cats = (box.categories ??= {});
      const qty = Math.max(1, entry.item.quantity);
      cats[entry.item.category] = (cats[entry.item.category] ?? 0) + qty;
      litres[target.id] = (litres[target.id] ?? 0) + need(entry.item);
      if (entry.fillAfter) {
        box.fillPct = entry.fillAfter.pct;
        box.fillSource = entry.fillAfter.source;
        box.fillCheckedAt = entry.fillAfter.at;
      }
      this.created += 1;
      return {
        entity: { id: `item-${this.created}`, name: entry.item.name },
        fieldsWritten: true,
        photoUploaded: true,
      };
    });
    // A known fill grows by what went in, as an estimate -- as the server does.
    for (const [id, l] of Object.entries(litres)) {
      const box = this.places.find((p) => p.id === id)?.box;
      if (!box?.capacityL || !box.fillSource || req.entries.some((e) => e.boxId === id && e.fillAfter)) continue;
      box.fillPct = (box.fillPct ?? 0) + (l / box.capacityL) * 100;
      box.fillSource = "estimated";
    }
    return { results };
  }
}

function need(item: ItemDraft): number {
  const d = item.dimensionsCm;
  const each = d ? (d.l * d.w * d.h) / 1000 : SIZE_LITRES[item.sizeBucket] ?? 8;
  return each * Math.max(1, item.quantity ?? 1);
}
