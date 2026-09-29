/**
 * A stand-in for react-native-keychain, mapped in for tests.
 *
 * The real module is a bridge to the iOS Keychain and cannot load outside a
 * native runtime. Backed by a Map keyed on service; `failNext` exists because
 * the interesting case is a Keychain that refuses, not one that works.
 */

/** service -> stored password. */
export const store = new Map<string, string>();
let failing = false;

export function resetStore(seed: Record<string, string> = {}): void {
  store.clear();
  failing = false;
  for (const [service, value] of Object.entries(seed)) store.set(service, value);
}

/** Makes the next read or write throw, as a locked or broken Keychain does. */
export function failNext(): void {
  failing = true;
}

function check(): void {
  if (failing) {
    failing = false;
    throw new Error("keychain unavailable");
  }
}

export enum ACCESSIBLE {
  WHEN_UNLOCKED_THIS_DEVICE_ONLY = "AccessibleWhenUnlockedThisDeviceOnly",
}

export async function getGenericPassword(options: { service: string }) {
  check();
  const password = store.get(options.service);
  return password === undefined ? false : { username: "connection", password, service: options.service };
}

export async function setGenericPassword(_username: string, password: string, options: { service: string }) {
  check();
  store.set(options.service, password);
  return { service: options.service, storage: "KC" };
}
