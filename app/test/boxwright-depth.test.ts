/**
 * The fake in this directory against the real module's index, because two
 * bugs in this app have already been found in fakes rather than in the code.
 * Types are checked by tsc (the fake imports the real specs); this checks the
 * runtime surface, by loading the REAL index with only Nitro's native entry
 * points replaced -- and, while it is loaded, that a build without the native
 * side degrades to "unsupported" instead of crashing at import.
 */
jest.mock("react-native-nitro-modules", () => ({
  NitroModules: {
    createHybridObject: () => {
      throw new Error("HybridObject DepthKit has not been registered");
    },
  },
  getHostComponent: (name: string, config: () => unknown) => ({ name, config: config() }),
  callback: (f: unknown) => (typeof f === "function" ? { f } : f),
}));

import * as real from "../modules/boxwright-depth/src/index";
// By path, for the reason offline.test.ts gives for the mmkv fake.
import * as fake from "./boxwright-depth";

beforeEach(() => fake.resetDepth());

test("the fake exports every value the real module does", () => {
  const names = Object.keys(real).sort();
  expect(names).toEqual(["DepthCamera", "DepthKit", "callback"]);
  for (const name of names) expect(fake).toHaveProperty(name);
});

test("the real view is registered under the name the native side uses, with its props", () => {
  const view = real.DepthCamera as unknown as { name: string; config: { validAttributes: Record<string, unknown> } };
  expect(view.name).toBe("DepthCamera");
  expect(Object.keys(view.config.validAttributes).sort()).toEqual(["active", "hybridRef", "mode", "onSessionEvent", "onStatus", "torch"]);
});

test("without the native side, the real module says unsupported rather than crashing", async () => {
  expect(real.DepthKit.isSupported).toBe(false);
  // Unknown, so the system camera asks for itself as it always has.
  expect(real.DepthKit.cameraAccess).toBe("undetermined");
  expect(fake.DepthKit.cameraAccess).toBe("undetermined");
  await expect(real.DepthKit.readFile("/tmp/a.depth")).rejects.toThrow(/not available/);
});

test("the fake is unsupported by default, like every phone without LiDAR", () => {
  expect(fake.DepthKit.isSupported).toBe(false);
  fake.resetDepth({ supported: true });
  expect(fake.DepthKit.isSupported).toBe(true);
});

test("the fake's readFile hands back the bytes, and rejects a missing file", async () => {
  fake.depthFiles.set("/tmp/a.depth", new Uint8Array([66, 87, 68, 49]));
  expect(new Uint8Array(await fake.DepthKit.readFile("/tmp/a.depth"))).toEqual(new Uint8Array([66, 87, 68, 49]));
  await expect(fake.DepthKit.readFile("/tmp/missing.depth")).rejects.toThrow(/no such file/);
});

test("callback wraps a function as Nitro does, and passes anything else through", () => {
  const fn = () => {};
  expect(fake.callback(fn)).toEqual({ f: fn });
  expect(real.callback(fn)).toEqual({ f: fn });
  expect(fake.callback(undefined)).toBeUndefined();
});
