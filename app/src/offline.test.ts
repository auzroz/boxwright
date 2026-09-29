/**
 * The offline queue is the one place a capture exists between being taken and
 * reaching Homebox, so every test here is about not losing one.
 *
 * `./photo` and `./api` are mocked as seams: the first pulls in the file and
 * image-editor native modules, which cannot load outside a native runtime,
 * and the second is the network. react-native-mmkv is replaced wholesale by
 * the fake in test/, which is also where a failing disk comes from.
 */
// Imported by PATH, not by module name: moduleNameMapper points
// "react-native-mmkv" at this same file, so both resolve to one module instance
// and the store a test seeds is the store offline.ts reads. By name, TypeScript
// would be checking against the real module's types instead of the fake's.
import { disk, failOn, keys, resetKv } from "../test/react-native-mmkv";
import * as keychain from "../test/react-native-keychain";
import { resetStore } from "../test/react-native-keychain";

import type { Box, CatalogResponse, QueuedCapture } from "./types";

jest.mock("./photo", () => ({
  capturePhotoUri: async (name?: string) => (name ? `file:///captures/${name}` : null),
  deleteCapturePhoto: jest.fn(),
  gcCapturePhotos: jest.fn(),
}));

jest.mock("./api", () => {
  const actual = jest.requireActual("./api");
  return {
    ...actual,
    catalog: jest.fn(),
    boxes: jest.fn(),
    categories: jest.fn(),
  };
});

import { ApiError, boxes, catalog, categories } from "./api";
import { deleteCapturePhoto, gcCapturePhotos } from "./photo";
import {
  cachedBoxes,
  discard,
  enqueue,
  flushQueue,
  loadAll,
  loadQueue,
  orderForOfflinePicks,
  refreshBoxCache,
  refreshCategoryCache,
  switchConnection,
  toCatalogEntry,
} from "./offline";
import { EMPTY_CONNECTION, currentConnection, saveConnection } from "./connection";

const QUEUE = "queue.v2";

const mockCatalog = catalog as jest.MockedFunction<typeof catalog>;

function capture(id: string, names: string[], photoName?: string): QueuedCapture {
  return {
    captureId: id,
    queuedAt: 1,
    entries: names.map((name, i) => ({
      item: {
        name,
        category: "tools",
        sizeBucket: "M",
        fragile: false,
        bulky: false,
        weightClass: "medium",
        notes: "",
        confidence: 0,
        quantity: 1,
      },
      entryId: `${id}-${i}`,
      boxId: "fresno",
      boxName: "Fresno",
    })),
    photoName,
    attempts: 0,
    lastError: "",
  };
}

function allLanded(n: number): CatalogResponse {
  return {
    results: Array.from({ length: n }, (_, i) => ({
      entity: { id: `e${i}`, name: `e${i}` },
      fieldsWritten: true,
      photoUploaded: true,
    })),
  };
}

function onDisk(): QueuedCapture[] {
  const raw = disk.get(QUEUE);
  return raw ? (JSON.parse(raw) as { entries: QueuedCapture[] }).entries : [];
}

beforeEach(() => {
  resetKv();
  mockCatalog.mockReset();
});

describe("reading the queue back", () => {
  test("an unreadable queue is quarantined, not overwritten", () => {
    resetKv({ [QUEUE]: "{not json" });
    loadQueue();

    const kept = keys().filter((k: string) => k.includes("queue.v2.broken-"));
    expect(kept).toHaveLength(1);
    expect(disk.get(kept[0] as string)).toBe("{not json");
  });

  test("an unreadable queue does NOT garbage-collect the photos it named", () => {
    resetKv({ [QUEUE]: "{not json" });
    // loadAll, because collecting is now its job: it is the only caller that
    // knows about BOTH the catalog queue and the pending list.
    loadAll();

    // Deleting every photo after quarantining the list of captures would turn
    // a recoverable problem into permanent loss.
    expect(gcCapturePhotos).not.toHaveBeenCalled();
  });

  test("an empty queue DOES collect orphaned photos", () => {
    resetKv({ [QUEUE]: JSON.stringify({ version: 2, entries: [] }) });
    loadAll();
    expect(gcCapturePhotos).toHaveBeenCalledWith(new Set());
  });
});

describe("queueing", () => {
  test("a double tap does not queue the same capture twice", () => {
    loadQueue();
    const c = capture("cap1", ["drill"]);
    enqueue(c);
    enqueue(c);
    expect(onDisk()).toHaveLength(1);
  });

  // "Saved on this phone" is shown on the strength of enqueue returning. A
  // full disk must surface as an error, never as a capture that exists only
  // in memory until the app is killed.
  test("a write the store refuses throws and changes nothing", () => {
    loadQueue();
    failOn(QUEUE);
    expect(() => enqueue(capture("cap1", ["drill"]))).toThrow("no space left");
    expect(onDisk()).toHaveLength(0);

    // Nor did it linger in memory: once the disk has room, the same capture
    // is accepted rather than skipped as a double tap of one already queued.
    resetKv();
    enqueue(capture("cap1", ["drill"]));
    expect(onDisk()).toHaveLength(1);
  });

  test("discarding removes the capture and its photo", () => {
    loadQueue();
    enqueue(capture("cap1", ["drill"], "cap1.jpg"));
    discard("cap1");
    expect(onDisk()).toHaveLength(0);
    expect(deleteCapturePhoto).toHaveBeenCalledWith("cap1.jpg");
  });
});

describe("flushing", () => {
  test("everything landing removes the capture and releases the photo", async () => {
    loadQueue();
    enqueue(capture("cap1", ["drill"], "cap1.jpg"));
    mockCatalog.mockResolvedValue(allLanded(1));

    await flushQueue();

    expect(onDisk()).toHaveLength(0);
    expect(deleteCapturePhoto).toHaveBeenCalledWith("cap1.jpg");
  });

  test("a partial failure keeps only what did not land, with its key intact", async () => {
    loadQueue();
    enqueue(capture("cap1", ["drill", "sander"], "cap1.jpg"));
    mockCatalog.mockResolvedValue({
      results: [
        { entity: { id: "e0", name: "drill" }, fieldsWritten: true, photoUploaded: true },
        { fieldsWritten: false, photoUploaded: false, error: "box not found" },
      ],
    });

    await flushQueue();

    const left = onDisk();
    expect(left).toHaveLength(1);
    expect(left[0]?.entries.map((e) => e.item.name)).toEqual(["sander"]);
    // The key travels with the entry. Recomputed at send time it would now be
    // index 0 -- the drill's key -- and the sander would be reported as
    // already filed and lost.
    expect(left[0]?.entries[0]?.entryId).toBe("cap1-1");
    // The photo is still needed by what is still queued.
    expect(deleteCapturePhoto).not.toHaveBeenCalledWith("cap1.jpg");
  });

  test("a capture discarded mid-flush is not filed", async () => {
    loadQueue();
    enqueue(capture("cap1", ["drill"]));
    enqueue(capture("cap2", ["sander"]));

    mockCatalog.mockImplementation(async () => {
      // While the first send is in flight the user discards the second.
      discard("cap2");
      return allLanded(1);
    });

    await flushQueue();

    expect(mockCatalog).toHaveBeenCalledTimes(1);
    expect(onDisk()).toHaveLength(0);
  });

  test("a transport failure stops the flush; a rejection of one entry does not", async () => {
    loadQueue();
    enqueue(capture("cap1", ["drill"]));
    enqueue(capture("cap2", ["sander"]));

    // Retriable: every remaining capture would fail the same way, each at a
    // 60s timeout. Status 0 is "never got a response at all".
    mockCatalog.mockRejectedValue(new ApiError("The backend did not respond.", 0));
    await flushQueue();
    expect(mockCatalog).toHaveBeenCalledTimes(1);

    // Not retriable and specific to one entry: one bad capture must not dam
    // every other capture behind it.
    mockCatalog.mockReset();
    mockCatalog.mockRejectedValue(new ApiError("box not found", 404));
    await flushQueue();
    expect(mockCatalog).toHaveBeenCalledTimes(2);
  });

  // Keeping a capture we cannot classify is strictly safer than discarding it,
  // so anything that is not an ApiError -- a platform error, or a bug in our
  // own wrapping -- counts as retriable.
  test("an unclassifiable error is treated as retriable", async () => {
    loadQueue();
    enqueue(capture("cap1", ["drill"]));
    enqueue(capture("cap2", ["sander"]));
    mockCatalog.mockRejectedValue(new Error("something we never wrapped"));

    await flushQueue();

    expect(mockCatalog).toHaveBeenCalledTimes(1);
    expect(onDisk()).toHaveLength(2);
  });

  test("the key is what goes on the wire, unchanged", () => {
    const entry = capture("cap1", ["drill"]).entries[0];
    expect(toCatalogEntry(entry!)).toMatchObject({ entryId: "cap1-0", boxId: "fresno" });
  });
});

describe("the offline picker refuses what the engine would exclude", () => {
  const box = (over: Partial<Box>): Box => ({
    id: over.id ?? "b",
    name: over.name ?? "Box",
    parentId: "",
    area: "",
    access: "",
    heavySafe: null,
    fragileSafe: null,
    capacityUnits: 8,
    usedUnits: 0,
    categories: {},
    gridX: 0,
    gridY: 0,
    isArea: false,
    ...over,
  });

  test("never a fragile item into a box recorded as crush-risk", () => {
    const list = [box({ id: "crush", name: "Crush", fragileSafe: false }), box({ id: "ok", name: "Ok" })];
    const [picks] = orderForOfflinePicks(list, [{ category: "kitchen", fragile: true }]);
    expect(picks?.map((b) => b.id)).toEqual(["ok"]);
  });

  test("unrecorded is unknown, and never excludes", () => {
    const list = [box({ id: "unknown", name: "Unknown" })];
    const [picks] = orderForOfflinePicks(list, [{ category: "kitchen", fragile: true }]);
    expect(picks?.map((b) => b.id)).toEqual(["unknown"]);
  });

  test("without the server's litres an item's size is unknown, and refuses nothing", () => {
    // An older backend sent no sizeLitres. Offline must not invent them.
    const list = [box({ id: "tight", name: "Tight", capacityL: 10, fillPct: 90, fillSource: "observed" })];
    const [picks] = orderForOfflinePicks(list, [{ category: "tools", sizeBucket: "XL" }], undefined);
    expect(picks?.map((b) => b.id)).toEqual(["tight"]);
  });

  test("an unrecorded capacity never refuses, however much the box already holds", () => {
    // The backend sends capacityUnits 0 for a box nobody measured. Offline used
    // to read that as 8, and a box with usedUnits past it as full.
    const list = [box({ id: "tools", name: "Tools", capacityUnits: 0, usedUnits: 24, itemCount: 12 })];
    const [picks] = orderForOfflinePicks(list, [{ category: "tools", sizeBucket: "XL" }]);
    expect(picks?.map((b) => b.id)).toEqual(["tools"]);
  });

  test("refusing everything falls back to the full list rather than a dead end", () => {
    const list = [box({ id: "crush", name: "Crush", fragileSafe: false })];
    const [picks] = orderForOfflinePicks(list, [{ category: "kitchen", fragile: true }]);
    expect(picks?.map((b) => b.id)).toEqual(["crush"]);
  });

  test("items from one photo see the space the ones before them took", () => {
    // Two 100 L containers seen empty, and two XL items of 60 L each: whichever
    // box the first goes into cannot also take the second.
    const seen = { capacityL: 100, fillPct: 0, fillSource: "observed" as const };
    const list = [box({ id: "small", name: "Small", ...seen }), box({ id: "other", name: "Other", ...seen })];
    const picks = orderForOfflinePicks(
      list,
      [
        { category: "tools", sizeBucket: "XL" },
        { category: "tools", sizeBucket: "XL" },
      ],
      { S: 2, M: 8, L: 25, XL: 60 },
    );
    expect(picks[0]?.[0]?.id).toBe("other");
    expect(picks[1]?.[0]?.id).toBe("small");
    // Whichever order they come out in, the two items must not both be told
    // to use the same box.
    expect(picks[0]?.[0]?.id).not.toBe(picks[1]?.[0]?.id);
  });
});

describe("the box read cache", () => {
  test("is empty, never a crash, when the file is nonsense", () => {
    resetKv({ "boxes.v1": "{not json" });
    expect(cachedBoxes()).toEqual([]);
  });
});

describe("switching servers", () => {
  const home = { ...EMPTY_CONNECTION, apiUrl: "https://box.example.com", apiToken: "one" };

  beforeEach(async () => {
    resetStore();
    await saveConnection(home);
    loadQueue();
  });

  // Queued box ids were chosen from THIS inventory. Sent elsewhere they are
  // "not found" at best, and a new-container entry would create a box in
  // somebody else's Homebox.
  test("a waiting capture blocks a move to a different inventory", async () => {
    enqueue(capture("c1", ["drill"]));
    await expect(switchConnection({ ...home, apiUrl: "https://other.example.com" })).rejects.toThrow(
      "still waiting to upload",
    );
    expect(currentConnection()).toEqual(home);
    expect(onDisk()).toHaveLength(1);
  });

  test("a rotated token goes through with captures waiting", async () => {
    enqueue(capture("c1", ["drill"]));
    await switchConnection({ ...home, apiToken: "two" });
    expect(currentConnection().apiToken).toBe("two");
    expect(onDisk()).toHaveLength(1);
  });

  test("a move with an empty queue forgets the old inventory's caches", async () => {
    resetKv({
      "boxes.v1": JSON.stringify({ version: 1, boxes: [{ id: "fresno" }] }),
      "categories.v1": JSON.stringify({ version: 1, categories: [] }),
    });
    loadQueue();
    await switchConnection({ ...home, homeboxToken: "hb_someone_else" });
    expect(currentConnection().homeboxToken).toBe("hb_someone_else");
    expect(cachedBoxes()).toEqual([]);
    expect(keys()).not.toContain("categories.v1");
  });

  test("keeping the destination keeps the caches", async () => {
    resetKv({ "boxes.v1": JSON.stringify({ version: 1, boxes: [{ id: "fresno" }] }) });
    loadQueue();
    await switchConnection({ ...home, apiToken: "two" });
    expect(cachedBoxes()).toHaveLength(1);
  });

  // Raised in review before the public release. A refresh asked of the old server can answer
  // after the switch; written, it would put the old inventory's boxes back as
  // the offline picks for the new one.
  test("a refresh from the server just left does not repopulate the caches", async () => {
    let answerBoxes: (v: { boxes: Box[]; stale: boolean }) => void = () => {};
    let answerCategories: (v: { categories: unknown[] }) => void = () => {};
    (boxes as jest.Mock).mockReturnValueOnce(new Promise((r) => (answerBoxes = r)));
    (categories as jest.Mock).mockReturnValueOnce(new Promise((r) => (answerCategories = r)));

    const boxRefresh = refreshBoxCache();
    const categoryRefresh = refreshCategoryCache();
    await switchConnection({ ...home, apiUrl: "https://other.example.com" });

    answerBoxes({ boxes: [{ id: "old-inventory-box" } as Box], stale: false });
    answerCategories({ categories: [{ key: "tools", label: "Tools", inUse: true }] });
    await boxRefresh;

    expect(cachedBoxes()).toEqual([]);
    expect(await categoryRefresh).toBeNull();
    expect(keys()).not.toContain("categories.v1");
  });

  // Also raised in review before the public release. The queue is checked before the Keychain
  // write, which is async; a capture queued in that gap belongs to the old
  // inventory, so the switch has to be undone rather than carry it across.
  test("a capture queued during the Keychain write undoes the switch", async () => {
    const real = keychain.setGenericPassword;
    let finishWrite: () => void = () => {};
    const spy = jest.spyOn(keychain, "setGenericPassword").mockImplementationOnce(
      (user, password, options) =>
        new Promise((resolve) => {
          finishWrite = () => resolve(real(user, password, options));
        }) as ReturnType<typeof real>,
    );

    const switching = switchConnection({ ...home, apiUrl: "https://other.example.com" });
    await Promise.resolve();
    enqueue(capture("late", ["drill"]));
    finishWrite();

    await expect(switching).rejects.toThrow("still waiting to upload");
    expect(currentConnection()).toEqual(home);
    expect(JSON.parse(keychain.store.values().next().value as string)).toEqual(home);
    expect(onDisk()).toHaveLength(1);
    spy.mockRestore();
  });

  // Raised again in review before the public release, against the fix above: switching first
  // and undoing it afterwards still left the new server current for as long
  // as the undoing Keychain write took, and a flush already running sent its
  // next capture there.
  test("a flush during the switch never reaches the new server", async () => {
    const real = keychain.setGenericPassword;
    const finishers: (() => void)[] = [];
    const spy = jest.spyOn(keychain, "setGenericPassword").mockImplementation(
      (user, password, options) =>
        new Promise((resolve) => {
          finishers.push(() => resolve(real(user, password, options)));
        }) as ReturnType<typeof real>,
    );
    const settle = () => new Promise<void>((resolve) => setTimeout(() => resolve(), 0));
    const sentTo: string[] = [];
    let answerFirst: (v: CatalogResponse) => void = () => {};
    mockCatalog.mockImplementationOnce(() => {
      sentTo.push(currentConnection().apiUrl);
      return new Promise((resolve) => (answerFirst = resolve));
    });
    mockCatalog.mockImplementationOnce(async () => {
      sentTo.push(currentConnection().apiUrl);
      return allLanded(1);
    });

    const switching = switchConnection({ ...home, apiUrl: "https://other.example.com" });
    const refused = expect(switching).rejects.toThrow("still waiting to upload");
    await settle();
    enqueue(capture("a", ["drill"]));
    enqueue(capture("b", ["saw"]));
    const flush = flushQueue();
    await settle();

    finishers.shift()?.(); // the switch's own write lands
    await settle();
    answerFirst(allLanded(1)); // and the flush moves on to its next capture
    await settle();
    while (finishers.length > 0) {
      finishers.shift()?.();
      await settle();
    }
    await flush;
    await refused;

    expect(sentTo).toEqual([home.apiUrl, home.apiUrl]);
    expect(currentConnection()).toEqual(home);
    spy.mockRestore();
  });
});
