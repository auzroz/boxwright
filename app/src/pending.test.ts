/**
 * A photo now has three possible owners: the pending list, the catalog queue,
 * and the capture on screen. Every test here is about the GC and the discard
 * paths not deleting a picture one of the others still needs.
 */
// By path, for the reason offline.test.ts gives.
import { disk, resetKv } from "../test/react-native-mmkv";

import type { ItemDraft, PendingCapture, QueuedCapture } from "./types";

jest.mock("./photo", () => ({
  capturePhotoUri: async (name?: string) =>
    name && (globalThis as Record<string, unknown>).__photos instanceof Set &&
    ((globalThis as Record<string, unknown>).__photos as Set<string>).has(name)
      ? `file:///captures/${name}`
      : null,
  deleteCapturePhoto: jest.fn((name?: string) => {
    if (name) ((globalThis as Record<string, unknown>).__photos as Set<string>).delete(name);
  }),
  gcCapturePhotos: jest.fn((referenced: Set<string>) => {
    const photos = (globalThis as Record<string, unknown>).__photos as Set<string>;
    for (const name of [...photos]) if (!referenced.has(name)) photos.delete(name);
  }),
}));

jest.mock("./api", () => {
  const actual = jest.requireActual("./api");
  return { ...actual, identify: jest.fn(), boxes: jest.fn(), categories: jest.fn(), catalog: jest.fn() };
});

import { resetStore } from "../test/react-native-keychain";

import { ApiError, identify } from "./api";
import { EMPTY_CONNECTION, saveConnection } from "./connection";
import { gcCapturePhotos } from "./photo";
import {
  addPending,
  discardPending,
  loadAll,
  loadPending,
  pendingPhotoNames,
  retryPending,
  runIdentification,
  readPending,
  releasePending,
} from "./offline";

const PENDING = "pending.v1";
const QUEUE = "queue.v2";

const mockIdentify = identify as jest.MockedFunction<typeof identify>;

function photosOnDisk(): Set<string> {
  return (globalThis as Record<string, unknown>).__photos as Set<string>;
}

function setPhotos(...names: string[]): void {
  (globalThis as Record<string, unknown>).__photos = new Set(names);
}

const draft = (name: string): ItemDraft => ({
  name,
  category: "tools",
  sizeBucket: "M",
  fragile: false,
  bulky: false,
  weightClass: "medium",
  notes: "",
  confidence: 0.9,
  quantity: 1,
});

function parked(id: string, over: Partial<PendingCapture> = {}): PendingCapture {
  return {
    captureId: id,
    queuedAt: 1,
    photoName: `${id}.jpg`,
    status: "identifying",
    attempts: 0,
    ...over,
  };
}

function onDisk(): PendingCapture[] {
  const raw = disk.get(PENDING);
  return raw ? (JSON.parse(raw) as { captures: PendingCapture[] }).captures : [];
}

beforeEach(() => {
  resetKv();
  setPhotos();
  mockIdentify.mockReset();
});

describe("the photo GC, now that three things can own a photo", () => {
  test("a parked capture's photo survives a restart", () => {
    resetKv({
      [PENDING]: JSON.stringify({
        version: 1,
        captures: [parked("cap1", { status: "ready", items: [draft("drill")] })],
      }),
      [QUEUE]: JSON.stringify({ version: 2, entries: [] }),
    });
    setPhotos("cap1.jpg");

    loadAll();

    expect(gcCapturePhotos).toHaveBeenCalledWith(new Set(["cap1.jpg"]));
    expect(photosOnDisk().has("cap1.jpg")).toBe(true);
  });

  test("a photo owned by the catalog queue survives too, and both are kept together", () => {
    const queued: QueuedCapture = {
      captureId: "q1",
      queuedAt: 1,
      entries: [],
      photoName: "q1.jpg",
      attempts: 0,
      lastError: "",
    };
    resetKv({
      [PENDING]: JSON.stringify({ version: 1, captures: [parked("cap1")] }),
      [QUEUE]: JSON.stringify({ version: 2, entries: [queued] }),
    });
    setPhotos("cap1.jpg", "q1.jpg", "orphan.jpg");

    loadAll();

    expect(photosOnDisk().has("cap1.jpg")).toBe(true);
    expect(photosOnDisk().has("q1.jpg")).toBe(true);
    // Referenced by nobody, which is the only thing the GC is for.
    expect(photosOnDisk().has("orphan.jpg")).toBe(false);
  });

  // The ordering bug this is here to prevent: read the queue first, GC, and
  // every parked capture loses its picture on the next launch.
  test("loadAll reads the pending list BEFORE running the GC", () => {
    resetKv({
      [PENDING]: JSON.stringify({ version: 1, captures: [parked("cap1")] }),
      [QUEUE]: JSON.stringify({ version: 2, entries: [] }),
    });
    setPhotos("cap1.jpg");
    loadAll();
    const referenced = (gcCapturePhotos as jest.Mock).mock.calls[0]?.[0] as Set<string>;
    expect(referenced.has("cap1.jpg")).toBe(true);
  });
});

describe("discarding and taking up", () => {
  test("discarding deletes the photo", () => {
    resetKv({ [QUEUE]: JSON.stringify({ version: 2, entries: [] }) });
    loadAll();
    setPhotos("cap1.jpg"); // taken after startup, as a real one always is
    addPending(parked("cap1"));

    discardPending("cap1");

    expect(onDisk()).toHaveLength(0);
    expect(photosOnDisk().has("cap1.jpg")).toBe(false);
  });

  // The one case where discarding must NOT delete: the same photo is already
  // in the catalog queue, waiting to be filed.
  test("discarding leaves a photo the catalog queue still needs", () => {
    const queued: QueuedCapture = {
      captureId: "cap1",
      queuedAt: 1,
      entries: [],
      photoName: "cap1.jpg",
      attempts: 0,
      lastError: "",
    };
    resetKv({ [QUEUE]: JSON.stringify({ version: 2, entries: [queued] }) });
    setPhotos("cap1.jpg");
    loadAll(); // the queue already claims it, so the GC keeps it
    addPending(parked("cap1"));

    discardPending("cap1");

    expect(photosOnDisk().has("cap1.jpg")).toBe(true);
  });

  // The durability rule: opening something must not make it LESS safe. The
  // drafts on the review screen exist only in React state, so a capture
  // removed from the list on open would be gone if the app were killed there,
  // and its photo collected on the next launch.
  test("a capture being reviewed is still claimed", () => {
    resetKv({ [QUEUE]: JSON.stringify({ version: 2, entries: [] }) });
    loadAll();
    setPhotos("cap1.jpg");
    addPending(parked("cap1", { status: "ready", items: [draft("drill")] }));

    const opened = readPending("cap1");
    expect(opened?.items?.[0]?.name).toBe("drill");

    // Still on the list, so a relaunch right now finds it and keeps the photo.
    expect(onDisk()).toHaveLength(1);
    loadAll();
    expect(photosOnDisk().has("cap1.jpg")).toBe(true);
    expect(readPending("cap1")?.items?.[0]?.name).toBe("drill");
  });

  test("releasing it once it has been dealt with drops it, keeping the photo", () => {
    resetKv({ [QUEUE]: JSON.stringify({ version: 2, entries: [] }) });
    loadAll();
    setPhotos("cap1.jpg");
    addPending(parked("cap1", { status: "ready", items: [draft("drill")] }));

    releasePending("cap1");

    expect(onDisk()).toHaveLength(0);
    // clearCapture owns the file from here, not this.
    expect(photosOnDisk().has("cap1.jpg")).toBe(true);
  });

  test("reading or releasing one that is not there is not a crash", () => {
    loadAll();
    expect(readPending("nope")).toBeUndefined();
    expect(() => releasePending("nope")).not.toThrow();
  });
});

// A LiDAR depth file sits beside the photo in captures/, so the GC sees it
// like any other file. Unlike the photo it has exactly one owner -- the pending
// list -- and never enters the catalog queue.
describe("a capture's depth file", () => {
  const withDepth = (id: string, over: Partial<PendingCapture> = {}) =>
    parked(id, { depthName: `${id}.depth`, ...over });

  test("survives a restart while its capture is pending", () => {
    resetKv({
      [PENDING]: JSON.stringify({ version: 1, captures: [withDepth("cap1", { status: "ready", items: [] })] }),
      [QUEUE]: JSON.stringify({ version: 2, entries: [] }),
    });
    setPhotos("cap1.jpg", "cap1.depth", "gone.depth");

    loadAll();

    expect(gcCapturePhotos).toHaveBeenCalledWith(new Set(["cap1.jpg", "cap1.depth"]));
    expect(photosOnDisk().has("cap1.depth")).toBe(true);
    // One whose capture is no longer listed is nobody's.
    expect(photosOnDisk().has("gone.depth")).toBe(false);
  });

  test("is named by pendingPhotoNames", () => {
    resetKv({ [PENDING]: JSON.stringify({ version: 1, captures: [withDepth("cap1"), parked("cap2")] }) });
    loadPending();
    expect(pendingPhotoNames()).toEqual(new Set(["cap1.jpg", "cap1.depth", "cap2.jpg"]));
  });

  test("is deleted when the capture is discarded", () => {
    resetKv({ [QUEUE]: JSON.stringify({ version: 2, entries: [] }) });
    loadAll();
    setPhotos("cap1.jpg", "cap1.depth");
    addPending(withDepth("cap1"));

    discardPending("cap1");

    expect(photosOnDisk().size).toBe(0);
  });

  // Even when the catalog queue still holds the PHOTO: the queue never holds
  // the depth file, so nothing else can be relying on it.
  test("is deleted on discard even when the queue keeps the photo", () => {
    const queued: QueuedCapture = {
      captureId: "cap1",
      queuedAt: 1,
      entries: [],
      photoName: "cap1.jpg",
      attempts: 0,
      lastError: "",
    };
    resetKv({ [QUEUE]: JSON.stringify({ version: 2, entries: [queued] }) });
    setPhotos("cap1.jpg", "cap1.depth");
    loadAll(); // the depth file is not listed yet, so the GC takes it...
    setPhotos("cap1.jpg", "cap1.depth"); // ...put it back as if taken after launch
    addPending(withDepth("cap1"));

    discardPending("cap1");

    expect(photosOnDisk()).toEqual(new Set(["cap1.jpg"]));
  });

  test("is deleted when the capture is released, while the photo is kept", () => {
    resetKv({ [QUEUE]: JSON.stringify({ version: 2, entries: [] }) });
    loadAll();
    setPhotos("cap1.jpg", "cap1.depth");
    addPending(withDepth("cap1", { status: "ready", items: [draft("drill")] }));

    releasePending("cap1");

    expect(onDisk()).toHaveLength(0);
    expect(photosOnDisk()).toEqual(new Set(["cap1.jpg"]));
  });

  test("a capture with no depth file discards and releases exactly as before", () => {
    resetKv({ [QUEUE]: JSON.stringify({ version: 2, entries: [] }) });
    loadAll();
    setPhotos("cap1.jpg", "cap2.jpg", "other.depth");
    addPending(parked("cap1"));
    addPending(parked("cap2", { status: "ready", items: [draft("drill")] }));

    expect(() => discardPending("cap1")).not.toThrow();
    expect(() => releasePending("cap2")).not.toThrow();

    // Nothing but cap1's photo went; a depth file nobody named was not touched.
    expect(photosOnDisk()).toEqual(new Set(["cap2.jpg", "other.depth"]));
  });

  test("never travels into the catalog queue", () => {
    // A compile-time guarantee: this line stops type-checking the day
    // QueuedCapture grows a depthName, so that change has to be deliberate --
    // and has to come with a GC and a discard path for the queue's copy.
    const queuedHasNoDepth: "depthName" extends keyof QueuedCapture ? false : true = true;
    expect(queuedHasNoDepth).toBe(true);
  });
});

describe("identifying what is waiting", () => {
  beforeEach(async () => {
    resetStore();
    await saveConnection({ ...EMPTY_CONNECTION, apiUrl: "https://box.example.com", apiToken: "t" });
    resetKv({ [QUEUE]: JSON.stringify({ version: 2, entries: [] }) });
    loadAll();
    setPhotos("cap1.jpg", "cap2.jpg");
  });

  // "Not connected" is not the photo's failure. Charged as one, three
  // launches with an unreadable Keychain set every parked photo aside.
  test("with no server configured, nothing is attempted or charged", async () => {
    await saveConnection(EMPTY_CONNECTION);
    addPending(parked("cap1"));

    await runIdentification();
    await runIdentification();
    await runIdentification();

    expect(mockIdentify).not.toHaveBeenCalled();
    expect(onDisk()[0]).toMatchObject({ status: "identifying", attempts: 0 });
  });

  test("works through the list oldest first and keeps the answers", async () => {
    mockIdentify.mockImplementation(async (uri: string) =>
      uri.includes("cap1") ? [draft("drill")] : [draft("sander")],
    );
    addPending(parked("cap1"));
    addPending(parked("cap2"));

    await runIdentification();

    const got = onDisk();
    expect(got.map((c) => c.status)).toEqual(["ready", "ready"]);
    expect(got[0]?.items?.[0]?.name).toBe("drill");
    expect(got[1]?.items?.[0]?.name).toBe("sander");
  });

  test("a backend that is down leaves everything to be retried, not failed", async () => {
    mockIdentify.mockRejectedValue(new ApiError("The backend did not respond.", 0));
    addPending(parked("cap1"));
    addPending(parked("cap2"));

    await runIdentification();

    // Still identifying, so the next foreground picks them up. And it stopped
    // after the first: the second would fail the same way, at a 60s timeout.
    expect(onDisk().every((c) => c.status === "identifying")).toBe(true);
    expect(mockIdentify).toHaveBeenCalledTimes(1);
  });

  // 422 means "there is no AI provider" or "nothing here was recognised", and
  // the answer to both is the manual-entry form -- which is reached by being
  // READY with no items, not by failing. AI_PROVIDER=none is a configuration
  // this project promises stays fully usable.
  test("a refusal becomes an empty ready capture, which is manual entry", async () => {
    mockIdentify.mockRejectedValue(new ApiError("no AI provider configured", 422));
    addPending(parked("cap1"));

    await runIdentification();

    const got = onDisk()[0];
    expect(got?.status).toBe("ready");
    expect(got?.items).toEqual([]);
  });

  test("a genuinely broken capture is reported and waits for the user", async () => {
    mockIdentify.mockRejectedValue(new ApiError("image is not a JPEG", 400));
    addPending(parked("cap1"));

    await runIdentification();

    const got = onDisk()[0];
    expect(got?.status).toBe("failed");
    expect(got?.error).toContain("not a JPEG");
  });

  test("a failed capture can be tried again", async () => {
    mockIdentify.mockRejectedValueOnce(new ApiError("image is not a JPEG", 400));
    addPending(parked("cap1"));
    await runIdentification();
    expect(onDisk()[0]?.status).toBe("failed");

    mockIdentify.mockResolvedValue([draft("drill")]);
    retryPending("cap1");
    await runIdentification();

    expect(onDisk()[0]?.status).toBe("ready");
    expect(onDisk()[0]?.items?.[0]?.name).toBe("drill");
  });

  // The head-of-line bug: the loop always takes the OLDEST entry still marked
  // identifying, so one photo that never succeeds blocked every photo behind
  // it -- on every trigger, for the life of the install.
  test("one photo that always fails does not dam the ones behind it", async () => {
    mockIdentify.mockImplementation(async (uri: string) => {
      if (uri.includes("cap1")) throw new ApiError("upstream exploded", 502);
      return [draft("sander")];
    });
    addPending(parked("cap1"));
    addPending(parked("cap2"));

    // Each run stops at the blocker while it still has attempts left.
    for (let i = 0; i < 5; i++) await runIdentification();

    const [bad, good] = onDisk();
    expect(bad?.status).toBe("failed");
    expect(bad?.attempts).toBe(3); // gave up rather than counting forever
    expect(good?.status).toBe("ready");
    expect(good?.items?.[0]?.name).toBe("sander");
  });

  // A photo that is gone cannot be identified, ever. Saying so beats retrying
  // it on every foreground for the life of the install.
  test("a missing photo fails once rather than retrying forever", async () => {
    addPending(parked("gone"));
    await runIdentification();

    expect(onDisk()[0]?.status).toBe("failed");
    expect(onDisk()[0]?.error).toContain("no longer on the device");
    expect(mockIdentify).not.toHaveBeenCalled();
  });

  test("two runs at once do not identify the same photo twice", async () => {
    let resolve: (v: ItemDraft[]) => void = () => {};
    mockIdentify.mockImplementation(() => new Promise((r) => (resolve = r)));
    addPending(parked("cap1"));

    const a = runIdentification();
    const b = runIdentification();
    // The photo lookup is async on bare React Native, so identify is not
    // called in the same tick; resolving before it is would resolve nothing.
    while (mockIdentify.mock.calls.length === 0) await new Promise<void>((r) => setImmediate(r));
    resolve([draft("drill")]);
    await Promise.all([a, b]);

    expect(mockIdentify).toHaveBeenCalledTimes(1);
  });

  test("the same capture cannot be added twice", () => {
    mockIdentify.mockResolvedValue([]);
    addPending(parked("cap1"));
    addPending(parked("cap1"));
    expect(onDisk()).toHaveLength(1);
  });
});

describe("reading the list off disk", () => {
  test("a list that will not parse is an empty list, never a crash", () => {
    resetKv({ [PENDING]: "{not json" });
    loadPending();
    expect(pendingPhotoNames().size).toBe(0);
  });

  test("a capture killed mid-identification is picked up again", () => {
    resetKv({ [PENDING]: JSON.stringify({ version: 1, captures: [parked("cap1")] }) });
    loadPending();
    expect(pendingPhotoNames().has("cap1.jpg")).toBe(true);
  });
});

describe("a list that cannot be read", () => {
  // The worst outcome available: a file we could not parse turns into a dozen
  // photos nobody can ever get back. loadQueue has always refused to collect
  // against a reference set produced by a failure; loadPending was added
  // without that rule.
  test("does NOT collect the photos it was holding", () => {
    resetKv({
      [PENDING]: "{not json",
      [QUEUE]: JSON.stringify({ version: 2, entries: [] }),
    });
    setPhotos("cap1.jpg", "cap2.jpg");

    loadAll();

    expect(gcCapturePhotos).not.toHaveBeenCalled();
    expect(photosOnDisk().has("cap1.jpg")).toBe(true);
    expect(photosOnDisk().has("cap2.jpg")).toBe(true);
  });

  test("an object whose captures is not an array counts as unreadable too", () => {
    resetKv({
      [PENDING]: JSON.stringify({ version: 1 }),
      [QUEUE]: JSON.stringify({ version: 2, entries: [] }),
    });
    setPhotos("cap1.jpg");

    loadAll();

    expect(gcCapturePhotos).not.toHaveBeenCalled();
    expect(photosOnDisk().has("cap1.jpg")).toBe(true);
  });

  test("is kept aside rather than overwritten", () => {
    resetKv({
      [PENDING]: "{not json",
      [QUEUE]: JSON.stringify({ version: 2, entries: [] }),
    });
    loadAll();
    const kept = [...disk.keys()].filter((f) => f.includes("pending.v1.broken-"));
    expect(kept).toHaveLength(1);
    expect(disk.get(kept[0] as string)).toBe("{not json");
  });

  // And an unreadable CATALOG queue must stop the collection too, now that
  // the pending list can be the thing holding a photo.
  test("an unreadable catalog queue also stops the collection", () => {
    resetKv({
      [PENDING]: JSON.stringify({ version: 1, captures: [parked("cap1")] }),
      [QUEUE]: "{not json",
    });
    setPhotos("cap1.jpg");

    loadAll();

    expect(gcCapturePhotos).not.toHaveBeenCalled();
    expect(photosOnDisk().has("cap1.jpg")).toBe(true);
  });
});

describe("a capture with a LiDAR depth file", () => {
  beforeEach(async () => {
    resetStore();
    await saveConnection({ ...EMPTY_CONNECTION, apiUrl: "https://box.example.com", apiToken: "t" });
    resetKv({ [QUEUE]: JSON.stringify({ version: 2, entries: [] }) });
    loadAll();
  });

  test("is measured once identified, before it is marked ready", async () => {
    const { depthFiles } = jest.requireActual("boxwright-depth") as typeof import("../test/boxwright-depth");
    const { encodeDepth } = jest.requireActual("./measure/depthfile") as typeof import("./measure/depthfile");
    const { lookAt, renderDepth } = jest.requireActual("../test/synthdepth") as typeof import("../test/synthdepth");
    const frame = renderDepth({ boxes: [{ at: [0, 0], size: [0.3, 0.1, 0.2] }] }, lookAt([0, 0.05, 0], 0.8, 45, 0), { seed: 3 });
    depthFiles.set("/captures/cap9.depth", new Uint8Array(encodeDepth(frame)));
    setPhotos("cap9.jpg", "cap9.depth");

    mockIdentify.mockResolvedValue([draft("shoebox")]);
    addPending({ ...parked("cap9"), depthName: "cap9.depth" });
    await runIdentification();

    expect(onDisk()[0]?.status).toBe("ready");
    const item = onDisk()[0]?.items?.[0];
    expect(item?.dimensionsSource).toBe("lidar");
    expect(Math.abs((item?.dimensionsCm?.l ?? 0) - 30)).toBeLessThanOrEqual(1.5);
  });

  test("without one, the model's items stand", async () => {
    setPhotos("cap8.jpg");
    mockIdentify.mockResolvedValue([draft("shoebox")]);
    addPending(parked("cap8"));
    await runIdentification();
    expect(onDisk()[0]?.status).toBe("ready");
    expect(onDisk()[0]?.items?.[0]?.name).toBe("shoebox");
    expect(onDisk()[0]?.items?.[0]?.dimensionsSource).toBeUndefined();
  });
});
