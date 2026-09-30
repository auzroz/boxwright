// Where this app sends things, chosen on the phone rather than at build time.
//
// A build installed from TestFlight cannot know which server it belongs to,
// and anything inlined into the JavaScript bundle is readable by whoever
// unpacks the app, so the address and token are asked for on first launch.
//
// Stored in the iOS Keychain through react-native-keychain, because two of
// the four values are credentials. One Keychain item holds the whole
// connection as JSON, under its own service name, so it can never be confused
// with anything else the app might keep there later.

import { useSyncExternalStore } from "react";
import * as Keychain from "react-native-keychain";

export interface Connection {
  /** The Boxwright backend, e.g. https://boxwright.example.ts.net. No trailing slash. */
  apiUrl: string;
  /** BOXWRIGHT_API_TOKEN. Empty only for a backend on loopback, i.e. a simulator. */
  apiToken: string;
  /**
   * The user's own Homebox, sent as X-Boxwright-Homebox-Url. Empty means "the
   * backend's own Homebox", which is the self-hosted default. Only honoured by
   * a backend running with ALLOW_CLIENT_HOMEBOX=true.
   */
  homeboxUrl: string;
  /** Sent as X-Boxwright-Homebox-Token. May be set without homeboxUrl; see backend instance.go. */
  homeboxToken: string;
}

export const EMPTY_CONNECTION: Connection = { apiUrl: "", apiToken: "", homeboxUrl: "", homeboxToken: "" };

/** The Keychain service the connection is stored under. */
const KEYCHAIN_SERVICE = "app.boxwright.connection.v1";
/** Keychain items need a username; ours carries nothing, the JSON is the secret. */
const KEYCHAIN_ACCOUNT = "connection";

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

export type Parsed = { ok: true; value: string } | { ok: false; error: string };

/**
 * Turns what someone typed into a base URL, or says what is wrong with it.
 *
 * Deliberately a pattern rather than `new URL()`: React Native's URL is a
 * partial implementation whose accessors throw on some versions, and all that
 * is needed here is scheme, host and an optional path prefix.
 *
 * A scheme is required rather than guessed. Guessing http would send a token in
 * the clear to a host that speaks https; guessing https would fail against the
 * LAN backend most people start with. Either guess is wrong for someone, and
 * the error message costs less than the wrong guess.
 *
 * `kind` decides what happens to a trailing /api, because the two servers
 * disagree about it. The app appends /api/v1/... to the Boxwright address
 * itself, so there a pasted /api is a mistake that would 404 every call.
 * Homebox's base, as the backend expects it (HOMEBOX_BASE_URL), INCLUDES /api,
 * so there it is added when missing.
 */
export function normalizeServerUrl(raw: string, kind: "boxwright" | "homebox" = "boxwright"): Parsed {
  const trimmed = raw.trim();
  if (trimmed === "") return { ok: false, error: "Enter the server address." };
  const match = /^(https?):\/\/([^/?#\s]+)(\/[^?#\s]*)?$/i.exec(trimmed);
  if (!match) {
    if (/^https?:\/\//i.test(trimmed)) {
      return { ok: false, error: "Enter just the server address, with no spaces, ? or # in it." };
    }
    return /^[a-z][a-z0-9+.-]*:\/\//i.test(trimmed)
      ? { ok: false, error: "Use an http:// or https:// address." }
      : { ok: false, error: "Start the address with http:// or https://, e.g. https://boxwright.example.com" };
  }
  const scheme = (match[1] ?? "").toLowerCase();
  const host = (match[2] ?? "").toLowerCase();
  let path = (match[3] ?? "").replace(/\/+$/, "");
  if (kind === "boxwright") {
    if (/\/api(\/v1)?$/i.test(path)) {
      return { ok: false, error: "Enter the server address without /api at the end." };
    }
  } else {
    path = path.replace(/\/api(\/v1)?$/i, "") + "/api";
  }
  return { ok: true, value: `${scheme}://${host}${path}` };
}

/**
 * The host part of a normalized URL, without port or credentials.
 * Exported for the transport rules, which are about the name, not the port.
 */
export function hostOf(url: string): string {
  const authority = /^https?:\/\/([^/]+)/i.exec(url)?.[1] ?? "";
  const withoutUser = authority.slice(authority.lastIndexOf("@") + 1);
  // An IPv6 literal keeps its brackets; strip the port after them.
  if (withoutUser.startsWith("[")) return withoutUser.slice(0, withoutUser.indexOf("]") + 1);
  return withoutUser.replace(/:\d+$/, "");
}

/**
 * True for a name iOS lets the app reach over plain http.
 *
 * The app ships with App Transport Security left on, plus
 * NSAllowsLocalNetworking. That permits http to IP address literals, `.local`
 * names and unqualified names (no dots) -- the LAN backend -- and nothing
 * else. A fully qualified name such as a Tailscale MagicDNS host
 * (`box.tailnet.ts.net`) is NOT local to iOS, whatever network it is on, and
 * needs https. `tailscale serve` provides exactly that.
 */
export function isLocalHost(host: string): boolean {
  const h = host.toLowerCase();
  if (h === "") return false;
  if (h.startsWith("[")) return true; // IPv6 literal
  if (/^\d{1,3}(\.\d{1,3}){3}$/.test(h)) return true; // IPv4 literal
  if (h.endsWith(".local")) return true;
  return !h.includes(".");
}

/**
 * True for this phone itself. Plain http to it never leaves the device, so
 * there is no network for a token to cross -- the Simulator talking to a
 * backend on the same Mac is the everyday case.
 */
export function isLoopback(host: string): boolean {
  const h = host.toLowerCase();
  return h === "localhost" || h === "[::1]" || /^127(\.\d{1,3}){3}$/.test(h);
}

/** What to tell someone about how their token will travel. Empty when https. */
export function transportWarnings(connection: Connection): string[] {
  const warnings: string[] = [];
  for (const [label, url] of [
    ["Boxwright server", connection.apiUrl],
    ["Homebox", connection.homeboxUrl],
  ] as const) {
    if (!url.toLowerCase().startsWith("http://")) continue;
    if (isLoopback(hostOf(url))) continue;
    if (!isLocalHost(hostOf(url))) {
      warnings.push(
        `iOS only allows plain http:// to local addresses (an IP address or a .local name). Use https:// for the ${label} — for example with \`tailscale serve\`.`,
      );
    } else {
      warnings.push(`The ${label} address is http://, so its token crosses the network unencrypted.`);
    }
  }
  return warnings;
}

/** Accepts a token pasted with its header prefix, which is how it appears in most docs. */
export function cleanToken(raw: string): string {
  return raw.trim().replace(/^bearer\s+/i, "");
}

export type Validated =
  | { ok: true; connection: Connection }
  | { ok: false; errors: Partial<Record<keyof Connection, string>> };

/**
 * Checks a whole draft from the settings screen.
 *
 * Mirrors the backend's own rules for client credentials so they are caught
 * on the form rather than as a 400 on the first capture: a Homebox URL without
 * a token is refused (it would pair the operator's credential with a host the
 * caller named), while a token alone is fine and reuses the backend's URL.
 */
export function validateConnection(draft: Connection): Validated {
  const errors: Partial<Record<keyof Connection, string>> = {};
  const api = normalizeServerUrl(draft.apiUrl);
  if (!api.ok) errors.apiUrl = api.error;

  let homeboxUrl = "";
  if (draft.homeboxUrl.trim() !== "") {
    const hb = normalizeServerUrl(draft.homeboxUrl, "homebox");
    if (hb.ok) homeboxUrl = hb.value;
    else errors.homeboxUrl = hb.error;
  }
  const homeboxToken = cleanToken(draft.homeboxToken);
  if (draft.homeboxUrl.trim() !== "" && homeboxToken === "") {
    errors.homeboxToken = "A Homebox address needs its token too.";
  }

  if (Object.keys(errors).length > 0) return { ok: false, errors };
  return {
    ok: true,
    connection: {
      apiUrl: api.ok ? api.value : "",
      apiToken: cleanToken(draft.apiToken),
      homeboxUrl,
      homeboxToken,
    },
  };
}

export function isConfigured(connection: Connection): boolean {
  return connection.apiUrl !== "";
}

/**
 * Whether two connections file into the same inventory.
 *
 * The API token is left out on purpose: rotating it changes who may use the
 * backend, not which Homebox the backend writes to, so queued captures and
 * cached boxes stay valid. A different Homebox token is treated as a different
 * inventory, even though it might be the same account with a fresh key --
 * the safe mistake is refusing to switch, never filing into the wrong place.
 */
export function sameDestination(a: Connection, b: Connection): boolean {
  return a.apiUrl === b.apiUrl && a.homeboxUrl === b.homeboxUrl && a.homeboxToken === b.homeboxToken;
}

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

interface ConnectionState {
  connection: Connection;
  /** False until the Keychain has been read; the app must not decide "unconfigured" before then. */
  loaded: boolean;
}

let state: ConnectionState = { connection: EMPTY_CONNECTION, loaded: false };
const listeners = new Set<() => void>();

function setState(next: ConnectionState): void {
  state = next;
  for (const listener of listeners) listener();
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

export function useConnection(): ConnectionState {
  return useSyncExternalStore(subscribe, () => state);
}

/** What every request is sent with. Synchronous so api.ts needs no await per call. */
export function currentConnection(): Connection {
  return state.connection;
}

/**
 * Reads the saved connection once at startup.
 *
 * Anything unreadable leaves the app unconfigured rather than throwing: a
 * corrupt entry is fixed by filling in the settings screen again, and there is
 * nothing in it that cannot be typed a second time.
 */
export async function loadConnection(): Promise<Connection> {
  let connection = EMPTY_CONNECTION;
  try {
    const saved = await Keychain.getGenericPassword({ service: KEYCHAIN_SERVICE });
    if (saved) {
      const parsed = JSON.parse(saved.password) as Partial<Connection>;
      const checked = validateConnection({
        apiUrl: String(parsed.apiUrl ?? ""),
        apiToken: String(parsed.apiToken ?? ""),
        homeboxUrl: String(parsed.homeboxUrl ?? ""),
        homeboxToken: String(parsed.homeboxToken ?? ""),
      });
      if (checked.ok) connection = checked.connection;
    }
  } catch (err) {
    console.warn("could not read the saved connection:", err instanceof Error ? err.message : String(err));
  }
  setState({ connection, loaded: true });
  return connection;
}

/**
 * Stores a validated connection and makes it current.
 *
 * Callers that care about the offline queue go through offline.switchConnection,
 * which refuses to move captures to a different inventory. This is the raw write.
 *
 * `beforeCommit` runs after the Keychain write and before the connection
 * becomes current, in the same synchronous step, so nothing can read the new
 * connection between the check and the switch. If it throws, the stored copy
 * is put back and the old connection stays current.
 */
export async function saveConnection(connection: Connection, beforeCommit: () => void = () => {}): Promise<void> {
  await writeConnection(connection);
  try {
    beforeCommit();
  } catch (err) {
    // No request has read the new settings; make the Keychain agree, so the
    // next launch does not load what was just refused.
    await writeConnection(state.connection);
    throw err;
  }
  setState({ connection, loaded: true });
}

async function writeConnection(connection: Connection): Promise<void> {
  const stored = await Keychain.setGenericPassword(KEYCHAIN_ACCOUNT, JSON.stringify(connection), {
    service: KEYCHAIN_SERVICE,
    // Readable only while the phone is unlocked, and never restored onto a
    // different device from a backup: these are credentials to someone's
    // inventory, and retyping them on a new phone is the cheap side of that.
    accessible: Keychain.ACCESSIBLE.WHEN_UNLOCKED_THIS_DEVICE_ONLY,
  });
  if (stored === false) throw new Error("The Keychain did not accept the settings.");
}
