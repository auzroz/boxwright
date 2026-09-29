/**
 * A stand-in for react-native-mmkv, mapped in for tests.
 *
 * The real module reaches native code through Nitro at import time. Backed by
 * a Map; `failOn` exists because the bugs worth pinning are the ones where a
 * WRITE fails and the recovery path does the wrong thing, and there is no
 * other way to produce a full disk on demand.
 */

/** key -> value. Exported so a test can seed or inspect the store. */
export const disk = new Map<string, string>();

/** Keys whose writes throw until reset, simulating a full or failing disk. */
const failing = new Set<string>();

export function resetKv(seed: Record<string, string> = {}): void {
  disk.clear();
  failing.clear();
  for (const [key, value] of Object.entries(seed)) disk.set(key, value);
}

export function failOn(key: string): void {
  failing.add(key);
}

export function keys(): string[] {
  return [...disk.keys()].sort();
}

export function createMMKV(_config: { id: string }) {
  return {
    getString(key: string): string | undefined {
      return disk.get(key);
    },
    set(key: string, value: string): void {
      if (failing.has(key)) throw new Error(`no space left on device: ${key}`);
      disk.set(key, value);
    },
    remove(key: string): boolean {
      return disk.delete(key);
    },
  };
}
