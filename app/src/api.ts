import { currentConnection, isConfigured } from "./connection";
import { DEMO_IDENTIFY_MS } from "./demo/backend";
import { demoActive, demoInventory } from "./demo/mode";
import type { Connection } from "./connection";

import type {
  AdoptResponse,
  BoxesResponse,
  CatalogEntry,
  CatalogEntryResult,
  CatalogRequest,
  CatalogResponse,
  CategoriesResponse,
  EntityTypesResponse,
  IdentifyResponse,
  ItemDraft,
  LocationChoice,
  LocationsResponse,
  LocationsWriteResponse,
  PutContainersRequest,
  PutContainersResponse,
  RecommendRequest,
  RecommendResponse,
  StatusResponse,
} from "./types";

/** Storage units have bad signal; a request that hangs forever is worse than one that fails. */
const TIMEOUT_MS = 60_000;

/**
 * The /api/v1 contract generation this build speaks. The backend reports its
 * own in GET /api/v1/status; see backend/internal/api/status.go.
 */
export const SUPPORTED_API_VERSION = 1;

/** Header names for per-request Homebox credentials; see backend/internal/api/instance.go. */
const HEADER_HOMEBOX_URL = "X-Boxwright-Homebox-Url";
const HEADER_HOMEBOX_TOKEN = "X-Boxwright-Homebox-Token";

/**
 * A failed call, carrying enough to decide whether retrying it later could
 * ever work.
 *
 * The offline queue needs that distinction and cannot get it from a message
 * string: "the backend did not respond" must be queued and retried, while
 * "box not found" must be shown to the user, because retrying it forever
 * would quietly hide a real problem behind a growing queue.
 */
export class ApiError extends Error {
  /** HTTP status, or 0 when the request never got a response at all. */
  readonly status: number;

  constructor(message: string, status: number) {
    super(message);
    this.name = "ApiError";
    this.status = status;
  }

  /**
   * True when nothing about the request itself was wrong, so the same bytes
   * sent again later stand a chance. Transport failures, timeouts, and the
   * server saying it is temporarily unable (including 502/503/504 from the
   * backend when Homebox is down) all qualify.
   */
  get retriable(): boolean {
    return this.status === 0 || this.status === 408 || this.status === 429 || this.status >= 500;
  }
}

/** True for anything worth putting on the offline queue rather than reporting. */
export function isRetriable(err: unknown): boolean {
  // A non-ApiError got past our own wrapping (a bug, or a platform error).
  // Treat it as retriable: keeping a capture we cannot classify is strictly
  // safer than discarding it.
  return err instanceof ApiError ? err.retriable : true;
}

/**
 * Did this entry reach Homebox?
 *
 * A missing result is treated as NOT landed on purpose. We cannot tell whether
 * an unreported entry was filed, and the two mistakes are not symmetrical:
 * keeping it risks a duplicate row somebody can delete, while assuming success
 * loses the capture for good.
 */
export function landed(result: CatalogEntryResult | undefined): boolean {
  return result !== undefined && (result.error ?? "") === "";
}

/** Why an entry did not land, in words that can go on screen. */
export function entryError(result: CatalogEntryResult | undefined): string {
  if (result === undefined) return "The server did not say what happened to this item.";
  return result.error ?? "";
}

export function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

async function asJson<T>(res: Response): Promise<T> {
  if (!res.ok) {
    let message = `HTTP ${res.status}`;
    try {
      const body = (await res.json()) as { error?: string };
      if (body.error) message = body.error;
    } catch {
      // keep the status message
    }
    throw new ApiError(message, res.status);
  }
  return (await res.json()) as T;
}

/**
 * The headers every request carries for a connection.
 *
 * Exported so the header rules are tested directly: the Homebox pair is sent
 * only when set, because the backend treats a present-but-empty header as a
 * request for client credentials and a backend that does not allow them
 * refuses the whole call.
 */
export function connectionHeaders(connection: Connection, base?: RequestInit["headers"]): Headers {
  const headers = new Headers(base);
  if (connection.apiToken) headers.set("Authorization", `Bearer ${connection.apiToken}`);
  if (connection.homeboxUrl) headers.set(HEADER_HOMEBOX_URL, connection.homeboxUrl);
  if (connection.homeboxToken) headers.set(HEADER_HOMEBOX_TOKEN, connection.homeboxToken);
  return headers;
}

/**
 * fetch with a timeout, and with an external signal chained in so a screen can
 * cancel in-flight work when the user backs out.
 *
 * The connection is read per call rather than captured at import, so a change
 * on the settings screen applies to the very next request. `options.connection`
 * overrides it for the one caller that must test settings before saving them.
 */
async function request(
  path: string,
  init: RequestInit,
  signal?: AbortSignal,
  options: { connection?: Connection; timeoutMs?: number } = {},
): Promise<Response> {
  const connection = options.connection ?? currentConnection();
  if (!isConfigured(connection)) {
    // Status 0 on purpose: nothing about the request is wrong, and a capture
    // taken before setup should wait rather than be thrown away.
    throw new ApiError("Boxwright is not connected to a server yet. Add one in Settings.", 0);
  }
  const ctl = new AbortController();
  const timer = setTimeout(() => ctl.abort(), options.timeoutMs ?? TIMEOUT_MS);
  const onAbort = () => ctl.abort();
  signal?.addEventListener("abort", onAbort);
  const headers = connectionHeaders(connection, init.headers);
  try {
    return await fetch(`${connection.apiUrl}${path}`, { ...init, headers, signal: ctl.signal });
  } catch (err) {
    // A caller-driven abort is a cancellation, not a failure, and must not
    // look like something the queue should retry.
    if (signal?.aborted) throw err;
    if (ctl.signal.aborted) {
      throw new ApiError("The backend did not respond. Check your connection and try again.", 0);
    }
    throw new ApiError(`Could not reach the backend: ${errorMessage(err)}`, 0);
  } finally {
    clearTimeout(timer);
    signal?.removeEventListener("abort", onAbort);
  }
}

/** What the settings screen shows after "Test connection". */
export type ProbeResult =
  | { ok: true; status: StatusResponse; warnings: string[] }
  | { ok: false; message: string };

/**
 * The demo's answer, after a moment: instant answers make the screens flash
 * past faster than anyone could follow, and the demo is for looking at them.
 * Honours an abort the way a real request would.
 */
function demoAnswer<T>(answer: () => T, signal?: AbortSignal, ms = 300): Promise<T> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      try {
        resolve(answer());
      } catch (err) {
        reject(err);
      }
    }, ms);
    signal?.addEventListener("abort", () => {
      clearTimeout(timer);
      const aborted = new Error("Aborted");
      aborted.name = "AbortError";
      reject(aborted);
    });
  });
}

/**
 * Checks a connection end to end WITHOUT saving it.
 *
 * One authenticated call, which answers three questions: is the backend there,
 * does it accept this token, and can it reach the Homebox these credentials
 * name. The failures are translated here because each one has a different fix
 * and the person reading them is standing somewhere with a phone, not a log.
 */
export async function probe(connection: Connection, signal?: AbortSignal): Promise<ProbeResult> {
  let status: StatusResponse;
  try {
    status = await asJson<StatusResponse>(
      await request("/api/v1/status", { method: "GET" }, signal, { connection, timeoutMs: 15_000 }),
    );
  } catch (err) {
    if (!(err instanceof ApiError)) return { ok: false, message: errorMessage(err) };
    switch (err.status) {
      case 0:
        return { ok: false, message: `${err.message}\n\nCheck the address, and that this phone can reach it — on the same network, or on your tailnet.` };
      case 401:
        return { ok: false, message: "The server is there, but it did not accept the token. Copy BOXWRIGHT_API_TOKEN from the server again." };
      case 404:
        return { ok: false, message: "Something answered, but it is not a Boxwright server this app can talk to — or it is an older one without /api/v1/status. Update the backend." };
      default:
        return { ok: false, message: err.message };
    }
  }

  const warnings: string[] = [];
  if ((status.apiVersion ?? 0) > SUPPORTED_API_VERSION) {
    warnings.push("This server is newer than this app. Update the app if anything misbehaves.");
  }
  if (!status.homebox?.ok) {
    warnings.push(`The server cannot reach Homebox: ${status.homebox?.error || "no reason given"}`);
  }
  // Identification being off is not a warning: the connection screen already
  // lists it among what works, and saying it twice read as a problem.
  if (!status.clientHomebox && (connection.homeboxUrl !== "" || connection.homeboxToken !== "")) {
    // Unreachable in practice -- such a request is refused with a 400 before
    // it gets here -- but worth saying if a proxy ever strips the headers.
    warnings.push("This server does not accept a Homebox of your own; it always uses its own.");
  }
  return { ok: true, status, warnings };
}

/**
 * The saved server's status. Unlike probe(), for use once the connection is
 * saved: it throws like any other request and leaves the wording to the caller.
 */
export async function status(signal?: AbortSignal): Promise<StatusResponse> {
  if (demoActive()) return demoAnswer(() => demoInventory().status(), signal);
  return asJson<StatusResponse>(await request("/api/v1/status", { method: "GET" }, signal, { timeoutMs: 15_000 }));
}

/**
 * The content type for a photo, from its file extension.
 *
 * Almost always JPEG, because persistCapturePhoto re-encodes every photo --
 * but when that fails the original is kept under its own extension, and it
 * may be a PNG or a HEIC. The backend passes the part's type straight to the vision model, and a PNG
 * labelled image/jpeg is refused by the stricter ones, which would push every
 * screenshot onto manual entry for no visible reason.
 */
export function imageTypeFor(uri: string): string {
  const ext = /\.([a-z0-9]+)$/i.exec(uri)?.[1]?.toLowerCase() ?? "";
  switch (ext) {
    case "png":
      return "image/png";
    case "gif":
      return "image/gif";
    case "heic":
      return "image/heic";
    case "webp":
      return "image/webp";
    default:
      return "image/jpeg";
  }
}

/**
 * A multipart file part for a local photo.
 *
 * React Native's networking layer reads `{uri, name, type}` off disk itself
 * and streams it into the request body, so a multi-megabyte photo never has
 * to pass through JS memory. The DOM typings only know Blob and string, hence
 * the cast; the shape is React Native's documented FormData contract. The name
 * carries the real extension because the backend names the Homebox
 * attachment after it.
 */
function filePart(uri: string): Blob {
  const type = imageTypeFor(uri);
  const ext = type === "image/jpeg" ? "jpg" : type.slice("image/".length);
  return { uri, name: `capture.${ext}`, type } as unknown as Blob;
}

/**
 * Send a photo for identification. Throws with a manual-entry hint when AI_PROVIDER=none.
 *
 * `uri` is expected to be a photo that has already been through
 * photo.persistCapturePhoto, which is the single intake point for a capture
 * and where the downscale to 1024px/q0.6 happens. Resizing here instead would
 * mean re-encoding a queued photo a second time on every flush attempt.
 */
export async function identify(uri: string, signal?: AbortSignal): Promise<ItemDraft[]> {
  if (demoActive()) return demoAnswer(() => demoInventory().identify(), signal, DEMO_IDENTIFY_MS);
  const form = new FormData();
  form.append("image", filePart(uri));
  // No Content-Type header: RN must set its own multipart boundary.
  const res = await asJson<IdentifyResponse>(
    await request("/api/v1/identify", { method: "POST", body: form }, signal),
  );
  // An empty list is a legal answer ("I see nothing I can name"), and the
  // caller turns it into a blank manual draft. Only null needs defending
  // against, and only because JSON makes that mistake easy to ship.
  return Array.isArray(res.items) ? res.items : [];
}

/**
 * Score every item from one photo, in ONE call.
 *
 * The call is batched for a reason that is not efficiency: the engine has to
 * see the items together, so that the second one competes for the capacity and
 * category counts the first just consumed. Calling this once per item would
 * recommend the same nearly-full tote to all of them.
 *
 * recommendations[i] belongs to items[i].
 */
export async function recommend(items: ItemDraft[], signal?: AbortSignal): Promise<RecommendResponse> {
  if (demoActive()) return demoAnswer(() => demoInventory().recommend(items), signal);
  const payload: RecommendRequest = { items };
  const res = await asJson<RecommendResponse>(
    await request(
      "/api/v1/recommend",
      { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(payload) },
      signal,
    ),
  );
  return { recommendations: Array.isArray(res.recommendations) ? res.recommendations : [], stale: res.stale === true };
}

/** The boxes the engine can choose between. */
export async function boxes(signal?: AbortSignal): Promise<BoxesResponse> {
  if (demoActive()) return demoAnswer(() => demoInventory().boxes(), signal);
  return asJson<BoxesResponse>(await request("/api/v1/boxes", { method: "GET" }, signal));
}

/**
 * The categories the user can file under: their live Homebox tags first, then
 * the unused seeds. This is the vocabulary shown to the picker AND to the
 * vision model, so the app must never substitute a list of its own for it.
 */
export async function categories(signal?: AbortSignal): Promise<CategoriesResponse> {
  if (demoActive()) return demoAnswer(() => demoInventory().categories(), signal);
  return asJson<CategoriesResponse>(await request("/api/v1/categories", { method: "GET" }, signal));
}

/**
 * Every location in the user's Homebox, with whether it is a placement target.
 *
 * This is the whole tree, not a filtered set: which of these Boxwright may
 * file into is the user's decision, and they cannot make it about locations we
 * never showed them.
 */
export async function locations(signal?: AbortSignal): Promise<LocationsResponse> {
  if (demoActive()) return demoAnswer(() => demoInventory().locations(), signal);
  const res = await asJson<LocationsResponse>(await request("/api/v1/locations", { method: "GET" }, signal));
  return {
    locations: Array.isArray(res.locations) ? res.locations : [],
    eligibleCount: res.eligibleCount ?? 0,
  };
}

/**
 * Record which locations Boxwright may file into.
 *
 * Send only what CHANGED. Each entry is a separate write to Homebox, and
 * re-sending the untouched ones would turn a two-box edit into a hundred
 * requests. Both directions have to be sent explicitly -- an omitted location
 * keeps whatever it had, so turning one off means sending `eligible: false`,
 * not leaving it out.
 *
 * A 200 does NOT mean every one landed; read `results`.
 */
export async function setLocations(
  choices: LocationChoice[],
  signal?: AbortSignal,
): Promise<LocationsWriteResponse> {
  if (demoActive()) return demoAnswer(() => demoInventory().setLocations(choices), signal);
  const res = await asJson<LocationsWriteResponse>(
    await request(
      "/api/v1/locations",
      { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ locations: choices }) },
      signal,
    ),
  );
  return { results: Array.isArray(res.results) ? res.results : [] };
}

/**
 * Record what the user knows about their containers -- "these are all one
 * kind, 100 L, 70x45x38 inside", or how full one is -- on several at once.
 * Only the fields present in `set` are written; nothing else is touched.
 *
 * A 200 does NOT mean every one landed; read `results`.
 */
export async function setContainers(
  body: PutContainersRequest,
  signal?: AbortSignal,
): Promise<PutContainersResponse> {
  if (demoActive()) return demoAnswer(() => demoInventory().setContainers(body), signal);
  const res = await asJson<PutContainersResponse>(
    await request(
      "/api/v1/containers",
      { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) },
      signal,
    ),
  );
  return { results: Array.isArray(res.results) ? res.results : [] };
}

/**
 * The one-time migration: adopt locations already set up as containers.
 *
 * For someone upgrading from a version that guessed at this, and for anyone
 * who filled the capacity and access fields in by hand. It never overrides a
 * decision already recorded, in either direction, and it is safe to run twice.
 */
export async function adoptLocations(signal?: AbortSignal): Promise<AdoptResponse> {
  if (demoActive()) return demoAnswer(() => demoInventory().adoptLocations(), signal);
  const res = await asJson<AdoptResponse>(
    await request("/api/v1/locations/adopt", { method: "POST" }, signal),
  );
  return { scanned: res.scanned ?? 0, marked: res.marked ?? 0, adopted: Array.isArray(res.adopted) ? res.adopted : [] };
}

/** Homebox entity types; a new container needs the id of one where isLocation is true. */
export async function entityTypes(signal?: AbortSignal): Promise<EntityTypesResponse> {
  if (demoActive()) return demoAnswer(() => demoInventory().entityTypes(), signal);
  return asJson<EntityTypesResponse>(await request("/api/v1/entity-types", { method: "GET" }, signal));
}

/**
 * File every item from one capture into Homebox, with the shared photo, in ONE
 * request.
 *
 * The items and the photo travel together so that the offline queue can
 * persist a single self-contained unit of work, rather than dependent requests
 * it would have to order and reconcile over bad signal -- and so that a photo
 * of eight things is uploaded once rather than eight times over one bar.
 *
 * results[i] belongs to entries[i]. The status code alone is not the answer:
 * 201 means at least one entry landed, so callers MUST read every result
 * before telling anyone this worked.
 */
export async function catalog(
  args: { entries: CatalogEntry[]; captureId?: string; capturedAt?: string },
  photoUri?: string,
  signal?: AbortSignal,
): Promise<CatalogResponse> {
  if (demoActive()) return demoAnswer(() => demoInventory().catalog({ entries: args.entries, captureId: args.captureId, capturedAt: args.capturedAt }), signal);
  const payload: CatalogRequest = { entries: args.entries, captureId: args.captureId, capturedAt: args.capturedAt };
  const form = new FormData();
  form.append("payload", JSON.stringify(payload));
  if (photoUri) form.append("image", filePart(photoUri));

  const res = await asJson<CatalogResponse>(
    await request("/api/v1/catalog", { method: "POST", body: form }, signal),
  );
  return { results: Array.isArray(res.results) ? res.results : [] };
}
