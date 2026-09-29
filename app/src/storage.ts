// The small, synchronous key-value store behind the offline queue and the
// read caches.
//
// Synchronous on purpose. The queue promises that a capture is on disk before
// enqueue() returns -- the done screen says "Saved on this phone" on the
// strength of it -- and every async file API available to a bare React Native
// app would turn that into "will be on disk shortly". MMKV writes through a
// memory-mapped file, so a set() has reached the kernel when it returns and
// survives the app being killed a moment later; each write is also
// checksummed, so a torn one is detected on the next launch rather than read
// as a truncated queue.
//
// Photos are NOT kept here. They are megabytes each and live as files; see
// photo.ts.

import { createMMKV } from "react-native-mmkv";

/**
 * One MMKV instance for everything the app persists except the Keychain.
 *
 * `recover-on-error`: when MMKV detects a checksum or length mismatch it
 * salvages what it can rather than discarding the whole store. A partly
 * recovered queue can be read, quarantined or retried; a discarded one is a
 * set of captures that silently never existed.
 */
const store = createMMKV({ id: "boxwright", recoveryStrategy: "recover-on-error" });

/** The stored text for `key`, or null when there is none. */
export function readText(key: string): string | null {
  return store.getString(key) ?? null;
}

/** Stores `value` as JSON under `key`. Throws when the store refuses the write. */
export function writeJson(key: string, value: unknown): void {
  store.set(key, JSON.stringify(value));
}

/** Removes `key`. Safe when it is already gone. */
export function remove(key: string): void {
  store.remove(key);
}

/**
 * Moves an unreadable value aside under a new key instead of overwriting it,
 * returning that key.
 *
 * We cannot use it, but it may still hold captures somebody can recover by
 * hand, and quietly clobbering it would be exactly the silent loss the queue
 * exists to prevent.
 */
export function quarantine(key: string): string | null {
  try {
    const value = store.getString(key);
    if (value === undefined) return null;
    const kept = `${key}.broken-${Date.now()}`;
    store.set(kept, value);
    store.remove(key);
    return kept;
  } catch {
    return null;
  }
}
