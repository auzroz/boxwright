/**
 * A stand-in for react-native-mmkv, mapped in for tests.
 *
 * The real module reaches native code through Nitro at import time. Backed by
 * a Map; `failOn` exists because the bugs worth pinning are the ones where a
 * WRITE fails and the recovery path does the wrong thing, and there is no
 * other way to produce a full disk on demand.
 */

/** One map per MMKV id, as on disk: two instances never share keys. */
const stores = new Map<string, Map<string, string>>();

function storeFor(id: string): Map<string, string> {
  let s = stores.get(id);
  if (!s) {
    s = new Map();
    stores.set(id, s);
  }
  return s;
}

/** The app's main store, key -> value. Exported so a test can seed or inspect it. */
export const disk = storeFor("boxwright");

/** Any store by id -- the demo's is "boxwright-demo". */
export function diskFor(id: string): Map<string, string> {
  return storeFor(id);
}

/** Keys whose writes throw until reset, simulating a full or failing disk. */
const failing = new Set<string>();

export function resetKv(seed: Record<string, string> = {}): void {
  for (const s of stores.values()) s.clear();
  failing.clear();
  for (const [key, value] of Object.entries(seed)) disk.set(key, value);
}

export function failOn(key: string): void {
  failing.add(key);
}

export function keys(): string[] {
  return [...disk.keys()].sort();
}

export function createMMKV(config: { id: string }) {
  const data = storeFor(config.id);
  return {
    getString(key: string): string | undefined {
      return data.get(key);
    },
    set(key: string, value: string): void {
      if (failing.has(key)) throw new Error(`no space left on device: ${key}`);
      data.set(key, value);
    },
    remove(key: string): boolean {
      return data.delete(key);
    },
    clearAll(): void {
      data.clear();
    },
  };
}
