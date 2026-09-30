/**
 * The demo's promise: nothing it takes can reach a real server, and leaving
 * it puts the real app back exactly as it was.
 */
import { disk, diskFor, resetKv } from "../../test/react-native-mmkv";
import { resetStore } from "../../test/react-native-keychain";

jest.mock("../photo", () => ({
  capturePhotoUri: async (name?: string) => (name ? `file:///captures/${name}` : null),
  deleteCapturePhoto: jest.fn(),
  gcCapturePhotos: jest.fn(),
  useDemoCaptures: jest.fn(),
  deleteDemoCaptures: jest.fn(async () => {}),
}));

import { identify } from "../api";
import { addPending, enqueue, flushQueue, loadAll, readPending } from "../offline";
import { deleteDemoCaptures, useDemoCaptures } from "../photo";
import { prefs, resetPrefsForTest } from "../prefs";
import { useDemoStore } from "../storage";
import type { QueuedCapture } from "../types";
import { demoItems } from "./backend";
import { demoActive, resetDemoForTest } from "./mode";
import { enterDemo, leaveDemo, resumeDemoIfOn } from "./session";

const realFetch = globalThis.fetch;

function capture(id: string): QueuedCapture {
  return {
    captureId: id,
    queuedAt: 1,
    entries: [{ item: demoItems()[0]!, entryId: `${id}-0`, boxId: "tools", boxName: "Tools" }],
    photoName: `${id}.jpg`,
    attempts: 0,
    lastError: "",
  };
}

function queued(store: Map<string, string>): string[] {
  const raw = store.get("queue.v2");
  return raw ? (JSON.parse(raw) as { entries: QueuedCapture[] }).entries.map((e) => e.captureId) : [];
}

beforeEach(() => {
  resetKv();
  resetStore();
  resetPrefsForTest();
  resetDemoForTest();
  useDemoStore(false);
  jest.clearAllMocks();
  // No real server exists in these tests; reaching for one is the failure.
  globalThis.fetch = jest.fn(async () => {
    throw new Error("the demo must not use the network");
  }) as unknown as typeof fetch;
  loadAll();
});

afterEach(() => {
  globalThis.fetch = realFetch;
});

test("a demo capture never lands in the real queue, and leaving deletes it", async () => {
  enqueue(capture("real-1"));
  expect(queued(disk)).toEqual(["real-1"]);

  enterDemo();
  expect(demoActive()).toBe(true);
  expect(useDemoCaptures).toHaveBeenLastCalledWith(true);
  enqueue(capture("demo-1"));
  addPending({ captureId: "demo-2", queuedAt: 1, photoName: "demo-2.jpg", status: "identifying", attempts: 0 });
  expect(queued(diskFor("boxwright-demo"))).toEqual(["demo-1"]);
  expect(queued(disk)).toEqual(["real-1"]);
  expect(disk.get("pending.v1") ?? "").not.toContain("demo-2");

  await leaveDemo();
  expect(demoActive()).toBe(false);
  expect(diskFor("boxwright-demo").size).toBe(0);
  expect(useDemoCaptures).toHaveBeenLastCalledWith(false);
  expect(deleteDemoCaptures).toHaveBeenCalled();
  // Back to the real world, as it was.
  expect(queued(disk)).toEqual(["real-1"]);
  expect(readPending("demo-2")).toBeUndefined();
});

test("inside the demo, filing goes to the sample inventory, not the network", async () => {
  enterDemo();
  enqueue(capture("demo-1"));
  await flushQueue();
  expect(globalThis.fetch).not.toHaveBeenCalled();
  expect(queued(diskFor("boxwright-demo"))).toEqual([]);
});

test("identification answers from the sample, whatever the photo", async () => {
  enterDemo();
  const items = await identify("file:///x.jpg");
  expect(items.map((i) => i.name)).toEqual(["Cordless drill", "Mason jars", "String lights"]);
  expect(globalThis.fetch).not.toHaveBeenCalled();
});

test("the demo is remembered across a relaunch, and forgotten on leaving", async () => {
  enterDemo();
  expect(prefs().mode).toBe("demo");
  resetDemoForTest();
  useDemoStore(false);
  expect(resumeDemoIfOn()).toBe(true);
  expect(demoActive()).toBe(true);
  await leaveDemo();
  expect(prefs().mode).toBeUndefined();
  expect(resumeDemoIfOn()).toBe(false);
});
