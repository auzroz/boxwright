// Everything the app can still do with no route to the backend.
//
// This is the reason the product exists: a storage unit has no signal, and a
// capture taken there has to survive the walk back out. Two pieces of state
// live on disk for that -- the queue of captures waiting to be filed, and the
// last box list we managed to fetch, without which an offline capture could
// not even name a destination and so could never be queued at all.
//
// Homebox stays the sole system of record. None of this is inventory: the
// queue is work not yet handed over, and the box and category lists are read
// caches of what Homebox already holds. All three live in storage.ts.

import { useSyncExternalStore } from "react";

import {
  ApiError,
  boxes as fetchBoxes,
  identify,
  categories as fetchCategories,
  catalog,
  entryError,
  errorMessage,
  isRetriable,
  landed,
} from "./api";
import { canHold, measuredDims, needLitres, withLitres } from "./capacity";
import { attachLidarDimensions } from "./lidar";
import type { SizeLitres } from "./capacity";
import { normalizeCategory, sanitizeCategories } from "./categories";
import { currentConnection, isConfigured, sameDestination, saveConnection } from "./connection";
import type { Connection } from "./connection";
import { capturePhotoUri, deleteCapturePhoto, gcCapturePhotos } from "./photo";
import { quarantine, readText, remove, writeJson } from "./storage";
import type {
  Box,
  CatalogEntry,
  CatalogEntryResult,
  CategoryOption,
  ContainerType,
  Dims,
  DimensionsSource,
  FillObservation,
  ItemDraft,
  NewContainerRequest,
  PendingCapture,
  PendingState,
  QueueState,
  QueuedCapture,
  QueuedEntry,
  SizeBucket,
} from "./types";

const QUEUE_KEY = "queue.v2";
const BOXES_KEY = "boxes.v1";
const CATEGORIES_KEY = "categories.v1";
/** Photos taken but not yet reviewed. See the pending section below. */
const PENDING_KEY = "pending.v1";

// ---------------------------------------------------------------------------
// Queue state
// ---------------------------------------------------------------------------

let state: QueueState = { entries: [], flushing: false, lastError: "", loaded: false };
const listeners = new Set<() => void>();

function setState(patch: Partial<QueueState>): void {
  state = { ...state, ...patch };
  for (const listener of listeners) listener();
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

/** Subscribes a component to the queue. No store library: this is one array. */
export function useQueue(): QueueState {
  return useSyncExternalStore(subscribe, () => state);
}

function persist(entries: QueuedCapture[]): void {
  writeJson(QUEUE_KEY, { version: 2, entries });
  setState({ entries });
}

/**
 * Takes one capture as it was read back, without judging it.
 *
 * Nothing is validated away: a capture we cannot make sense of is still
 * somebody's, and it is better to show it stuck with an error than to make it
 * disappear. Only the one thing the screens index into is defaulted.
 */
function adoptCapture(raw: unknown): QueuedCapture {
  const c = (raw ?? {}) as QueuedCapture;
  return Array.isArray(c.entries) ? c : { ...c, entries: [] };
}

/**
 * Reads both stored lists, then collects photos neither of them claims.
 *
 * The garbage collection lives HERE rather than inside either loader, because
 * it is the only place that knows about both. A photo in captures/ can be
 * owned by the catalog queue, by the pending list, or by the capture currently
 * on screen -- and the third is why this runs at startup only, when there is
 * no capture on screen.
 *
 * It is skipped entirely when EITHER list failed to load. A reference set built
 * from a failure names nothing, so collecting against it deletes every photo
 * both lists were holding -- turning a value we could not read into a dozen
 * pictures nobody can ever get back. Orphaned photos are cheap; a capture is
 * not.
 */
export function loadAll(): void {
  const pendingOK = loadPending();
  const queueOK = loadQueue();
  if (!pendingOK || !queueOK) return;

  const referenced = new Set<string>();
  for (const entry of state.entries) if (entry?.photoName) referenced.add(entry.photoName);
  for (const name of pendingPhotoNames()) referenced.add(name);
  gcCapturePhotos(referenced);
}

/**
 * Reads the queue from storage, reporting whether it could be read. Called by
 * loadAll, before anything can queue.
 */
export function loadQueue(): boolean {
  let entries: QueuedCapture[] = [];
  let lastError = "";
  // Tracks "the stored queue was unreadable" as distinct from "the queue is
  // empty". They look identical in `entries` and mean opposite things to the
  // photo GC below.
  let unreadable = false;

  try {
    const raw = readText(QUEUE_KEY);
    if (raw !== null) {
      const parsed = JSON.parse(raw) as { entries?: unknown };
      if (Array.isArray(parsed.entries)) entries = parsed.entries.map(adoptCapture);
      else unreadable = true; // a value we cannot interpret, not an empty queue
    }
  } catch (err) {
    unreadable = true;
    const kept = quarantine(QUEUE_KEY);
    lastError = kept
      ? `Could not read pending captures; they were kept as ${kept}.`
      : `Could not read pending captures: ${errorMessage(err)}`;
  }

  // An unreadable queue is reported to loadAll, which then collects no
  // photos: quarantining the queue keeps it for hand recovery, and deleting
  // every photo it named would turn a recoverable problem into permanent loss.
  setState({ entries, flushing: false, lastError, loaded: true });
  return !unreadable;
}

/** Adds a capture to the queue and writes it to disk before returning. */
export function enqueue(entry: QueuedCapture): void {
  // A double tap can deliver two presses before React re-renders, and both
  // would carry the same captureId -- duplicate rows, a duplicate React key,
  // and on flush the first success removes both while the loop still sends the
  // second. Idempotent by id is the robust guard; a disabled button is not.
  if (state.entries.some((e) => e.captureId === entry.captureId)) return;

  persist([...state.entries, entry]);
}

/**
 * Drops a capture at the user's explicit request. There is no other path that
 * removes an entry without it having reached Homebox first.
 */
export function discard(captureId: string): void {
  // Clearing the error with the last entry: flushQueue early-returns on an
  // empty queue, so a failure recorded before the user discarded the entry
  // would otherwise leave a "Queue problem" banner up for the whole session
  // with nothing to retry and no way to dismiss it.

  const entry = state.entries.find((e) => e.captureId === captureId);
  const remaining = state.entries.filter((e) => e.captureId !== captureId);
  persist(remaining);
  if (remaining.length === 0) setState({ lastError: "" });
  deleteCapturePhoto(entry?.photoName);
}

function recordFailure(captureId: string, message: string): void {
  persist(
    state.entries.map((e) =>
      e.captureId === captureId ? { ...e, attempts: e.attempts + 1, lastError: message } : e,
    ),
  );
}

/** A queued entry as /catalog wants it: boxName is ours, for the UI only. */
export function toCatalogEntry(entry: QueuedEntry): CatalogEntry {
  // entryId is passed through, never generated here. Generating it at send
  // time is exactly the bug: a partial flush re-sends a SUBSET of a capture's
  // entries, so anything derived from the send would key the same item
  // differently each attempt, and anything derived from its position would key
  // it as a different item entirely.
  return {
    item: entry.item,
    entryId: entry.entryId,
    boxId: entry.boxId,
    newContainer: entry.newContainer,
    fillAfter: entry.fillAfter,
  };
}

/**
 * Records how full a container was seen to be on the entries of a capture
 * that is still waiting to be filed, so the answer travels with them. Returns
 * whether any queued entry was going there; false means the items already
 * left, and the caller records the fill directly instead.
 */
export function setQueuedFill(captureId: string, boxId: string, fill: FillObservation): boolean {
  let found = false;
  const next = state.entries.map((c) => {
    if (c.captureId !== captureId) return c;
    const entries = c.entries.map((e) => {
      if (e.boxId !== boxId) return e;
      found = true;
      return { ...e, fillAfter: fill };
    });
    return { ...c, entries };
  });
  if (found) persist(next);
  return found;
}

/**
 * The first per-entry failure, named so the queue row says which item is stuck
 * rather than only that something is.
 */
function firstFailure(capture: QueuedCapture, results: (CatalogEntryResult | undefined)[]): string {
  for (let i = 0; i < capture.entries.length; i++) {
    const result = results[i];
    if (landed(result)) continue;
    const name = capture.entries[i]?.item?.name || "One item";
    return `${name}: ${entryError(result)}`;
  }
  return "";
}

// Guards against a second flush starting while one is in flight (app
// foregrounded while a manual retry is running), which would file duplicates
// for every entry the running flush has not yet removed.
let flushing = false;

/**
 * Tries to file every pending capture, oldest first.
 *
 * There is no connectivity check: whether the phone has bars says nothing
 * about whether this particular backend is reachable, and the request itself
 * is the only honest test. Cheap to call on any trigger.
 */
export async function flushQueue(): Promise<void> {
  if (flushing || !state.loaded || state.entries.length === 0) return;
  flushing = true;
  setState({ flushing: true, lastError: "" });
  try {
    for (const entry of [...state.entries]) {
      // The snapshot can go stale: an upload takes up to 60s, and the user can
      // Discard another entry while it runs. Filing something they were told
      // "cannot be undone" about is the worst thing this queue can do, so
      // re-check membership against live state before every send.
      if (!state.entries.some((e) => e.captureId === entry.captureId)) continue;
      try {
        const res = await catalog(
          {
            entries: entry.entries.map(toCatalogEntry),
            // Correlation only. What actually stops a retry filing a second
            // copy is the per-entry entryId inside each entry, because a
            // partial flush re-sends this same captureId with FEWER entries --
            // so anything keyed on the capture, or on a position within it,
            // would match the wrong item.
            captureId: entry.captureId,
            // When the items were filed on this phone. A fill observed after
            // it already includes them, so the server adds nothing to it.
            capturedAt: new Date(entry.queuedAt).toISOString(),
          },
          // A missing photo does not hold up the item. The entity landing in
          // Homebox is the durable outcome; the photo is an attachment on it.
          (await capturePhotoUri(entry.photoName)) ?? undefined,
        );

        // Per-entry outcomes, so a capture is never re-sent whole because one
        // of its items was rejected: what landed is removed, what did not
        // stays queued with its reason.
        const unfiled = entry.entries.filter((_, i) => !landed(res.results[i]));
        if (unfiled.length === 0) {
          persist(state.entries.filter((e) => e.captureId !== entry.captureId));
          // Every item is in Homebox, so this is the one moment the shared
          // photo is no longer needed by anybody.
          deleteCapturePhoto(entry.photoName);
          continue;
        }

        const reason = firstFailure(entry, res.results);
        persist(
          state.entries.map((e) =>
            e.captureId === entry.captureId
              ? { ...e, entries: unfiled, attempts: e.attempts + 1, lastError: reason }
              : e,
          ),
        );
        setState({ lastError: reason });
        // The request itself succeeded, so the backend is reachable and the
        // remaining captures are worth trying. Only a transport failure earns
        // the break below.
      } catch (err) {
        const message = errorMessage(err);
        recordFailure(entry.captureId, message);
        setState({ lastError: message });
        // Still offline, or the backend is down: every remaining entry would
        // fail the same way and each attempt costs a 60s timeout. Stop and
        // wait for the next trigger. A rejection specific to THIS entry (a
        // deleted box, say) blocks nothing -- carry on, so one bad entry
        // cannot dam every capture behind it.
        if (isRetriable(err)) break;
      }
    }
  } finally {
    flushing = false;
    setState({ flushing: false });
  }
}

/**
 * Only has to be unique within one device's queue, where two captures in the
 * same millisecond is already impossible by hand, so this is plenty and saves
 * pulling in a crypto dependency for it.
 */
export function newCaptureId(): string {
  return `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;
}

// ---------------------------------------------------------------------------
// Switching servers
// ---------------------------------------------------------------------------

/** Thrown when a server change would send waiting captures to the wrong inventory. */
export class ConnectionChangeBlocked extends Error {
  readonly pending: number;

  constructor(pending: number) {
    super(
      pending === 1
        ? "One capture is still waiting to upload to the current server. Let it upload, or discard it, before switching."
        : `${pending} captures are still waiting to upload to the current server. Let them upload, or discard them, before switching.`,
    );
    this.name = "ConnectionChangeBlocked";
    this.pending = pending;
  }
}

/**
 * Saves new connection settings, refusing any change that would re-point
 * waiting captures at a different inventory.
 *
 * Everything queued was decided against the current Homebox: its box ids, its
 * containers, its tags. Sent to another Homebox, a box id is at best "not
 * found" and at worst names something else entirely -- and a new-container
 * entry would quietly create a box in someone else's inventory. So a change of
 * destination waits for an empty queue. Nothing is dropped on the user's
 * behalf; they can let the queue drain, or discard deliberately.
 *
 * A change that keeps the destination (a rotated API token) goes straight
 * through, queue or not. A change that moves it also forgets the box and
 * category caches, which describe the old inventory and would otherwise be
 * offered as offline picks for the new one.
 */
export async function switchConnection(next: Connection): Promise<void> {
  // Parked captures deliberately do not block this. They hold a photo and,
  // once identified, drafts -- nothing that names a box or a container, so
  // nothing that belongs to one inventory. Whichever server identifies them,
  // the user reviews the result before anything is filed, and any category
  // the new server does not have is shown as new. An identification already
  // in flight finishes against the server it was sent to; request() reads
  // the connection per call, so the next one goes to the new server.
  const moving = !sameDestination(currentConnection(), next);
  const refuseWhileWaiting = () => {
    if (!moving) return;
    if (flushing) throw new ConnectionChangeBlocked(Math.max(1, state.entries.length));
    if (state.entries.length > 0) throw new ConnectionChangeBlocked(state.entries.length);
  };
  refuseWhileWaiting();

  // The Keychain write is async, and a capture queued during it was decided
  // against the OLD inventory. So the queue is checked again after the write
  // but before the new connection becomes current, in one synchronous step:
  // switching first and undoing it afterwards left a window in which a flush
  // could send that capture to the new server. Refusing here keeps enqueue
  // unable to fail. (Raised twice by review on PR #1.)
  await saveConnection(next, () => {
    refuseWhileWaiting();
    if (!moving) return;
    // Every refresh already in flight was asked of the old server; bumping
    // the epoch makes each of them discard its answer instead of writing it
    // into the caches just cleared for the new one.
    destinationEpoch += 1;
    forgetInventoryCaches();
  });
}

/**
 * Which inventory the read caches currently describe. A refresh records it
 * before it fetches and writes only if it is unchanged when the answer comes
 * back, so a slow response from the server the user just left can never
 * repopulate caches that now belong to another. Without this, an old box list
 * could survive a switch as the offline picks for a new inventory, and a
 * capture filed to one of those boxes would send an old box id to the new
 * Homebox.
 */
let destinationEpoch = 0;

/** Drops the read caches that describe one particular inventory. */
function forgetInventoryCaches(): void {
  for (const key of [BOXES_KEY, CATEGORIES_KEY]) {
    try {
      remove(key);
    } catch {
      // A stale cache that could not be deleted is overwritten by the next
      // successful refresh; not worth failing a settings save over.
    }
  }
}

// ---------------------------------------------------------------------------
// Box read cache
// ---------------------------------------------------------------------------

/**
 * Refreshes the offline box list. Failure is expected and silent: an old
 * snapshot is the whole point, and there is nothing for the user to do about
 * a refresh that could not happen.
 */
export async function refreshBoxCache(): Promise<void> {
  const epoch = destinationEpoch;
  try {
    const res = await fetchBoxes();
    if (epoch !== destinationEpoch) return; // answered by the server we left
    // The container types and the litres per size bucket ride along, so the
    // offline picker and the setup screen work from the server's own figures.
    // Same key and version: every addition is optional, and a new key would
    // throw away the snapshot a phone with no signal is relying on.
    writeJson(BOXES_KEY, {
      version: 1,
      boxes: res.boxes,
      containerTypes: res.containerTypes,
      sizeLitres: res.sizeLitres,
    });
  } catch {
    /* keep whatever snapshot we already have */
  }
}

/** Litres per size bucket from the last box list, when that backend sent them. */
export function cachedSizeLitres(): SizeLitres | undefined {
  try {
    const raw = readText(BOXES_KEY);
    if (raw === null) return undefined;
    const parsed = JSON.parse(raw) as { sizeLitres?: unknown };
    const v = parsed.sizeLitres;
    return v && typeof v === "object" ? (v as SizeLitres) : undefined;
  } catch {
    return undefined;
  }
}

/** The user's own container types from the last box list; [] when none. */
export function cachedContainerTypes(): ContainerType[] {
  try {
    const raw = readText(BOXES_KEY);
    if (raw === null) return [];
    const parsed = JSON.parse(raw) as { containerTypes?: unknown };
    return Array.isArray(parsed.containerTypes) ? (parsed.containerTypes as ContainerType[]) : [];
  } catch {
    return [];
  }
}

/** The last box list we managed to fetch. Empty before the first success. */
export function cachedBoxes(): Box[] {
  try {
    const raw = readText(BOXES_KEY);
    if (raw === null) return [];
    const parsed = JSON.parse(raw) as { boxes?: unknown };
    return Array.isArray(parsed.boxes) ? (parsed.boxes as Box[]) : [];
  } catch {
    return [];
  }
}

// ---------------------------------------------------------------------------
// Category read cache
// ---------------------------------------------------------------------------

/**
 * Refreshes the offline category list, returning it when it changed hands and
 * null when the fetch failed.
 *
 * Cached for the same reason the box list is: the vocabulary is the user's
 * Homebox tags, and without it a phone in a storage unit would fall back to our
 * eleven seeds and be unable to file under "Health" or "Micro Masterpieces" --
 * categories that already exist and already carry box affinity. Failure is
 * silent; an older list is exactly what the cache is for.
 */
export async function refreshCategoryCache(): Promise<CategoryOption[] | null> {
  const epoch = destinationEpoch;
  try {
    const res = await fetchCategories();
    if (epoch !== destinationEpoch) return null; // answered by the server we left
    const list = sanitizeCategories(res.categories);
    // An empty answer is never worth writing over a good older one. It is
    // indistinguishable from a backend that has lost its Homebox connection,
    // and adopting it would strip the user's own categories from the picker
    // until the next successful fetch.
    if (list.length === 0) return null;
    writeJson(CATEGORIES_KEY, { version: 1, categories: list });
    return list;
  } catch {
    return null;
  }
}

/** The last category list we managed to fetch. Empty before the first success. */
export function cachedCategories(): CategoryOption[] {
  try {
    const raw = readText(CATEGORIES_KEY);
    if (raw === null) return [];
    const parsed = JSON.parse(raw) as { categories?: unknown };
    return sanitizeCategories(parsed.categories);
  } catch {
    return [];
  }
}

/**
 * Orders cached boxes for a manual offline pick: ones that already hold this
 * category first, then by name.
 *
 * This is NOT the placement engine and must never be presented as its answer.
 * It scores nothing, weighs nothing, and knows nothing about capacity or
 * fragility -- it only saves the user scrolling. The real recommendation is
 * made server-side when there is a connection, and the UI says so.
 *
 * Internal: callers go through orderForOfflinePicks, because a photo yields
 * several items and they have to be ordered as a set.
 */
function orderForOfflinePick(list: Box[], category: string, item?: OfflineItem, sizeLitres?: SizeLitres): Box[] {
  const want = normalizeCategory(category);

  // The engine's hard exclusions are not scoring preferences -- they are the
  // only absolute rules it has: never a fragile item into a box recorded as
  // crush-risk, never a heavy item into one recorded as not heavy-safe, never
  // past capacity. Offline we cannot rank, but we CAN still refuse, and
  // silently offering a forbidden box would make "fragile items never into
  // crush-risk boxes" unenforceable on exactly the path used in the storage
  // unit. Tri-state matters: null means unrecorded, which never excludes.
  const need = item ? needLitres(item, sizeLitres) : undefined;
  const measured = item ? measuredDims(item) : undefined;
  const allowed = list.filter((b) => {
    if (item?.fragile && b.fragileSafe === false) return false;
    if (item?.weightClass === "heavy" && b.heavySafe === false) return false;
    // Containers only, and only on facts, exactly as the engine's canHold: a
    // recorded capacity, a measured size against a recorded inside, a fill
    // someone saw. Assuming a capacity for an unrecorded one is what once
    // read every container holding four things as full.
    if (!b.isArea && !canHold(b, need, measured)) return false;
    return true;
  });

  // Refusing everything would leave the user unable to file anything at all,
  // which is worse than an imperfect box. Fall back to the full list rather
  // than a dead end -- the same shape as the engine's new-container escape.
  const pool = allowed.length > 0 ? allowed : list;

  return [...pool].sort((a, b) => {
    const aHas = (a.categories?.[want] ?? 0) > 0 ? 1 : 0;
    const bHas = (b.categories?.[want] ?? 0) > 0 ? 1 : 0;
    if (aHas !== bHas) return bHas - aHas;
    return a.name.localeCompare(b.name);
  });
}

/** What orderForOfflinePicks needs to know about one item. */
type OfflineItem = {
  category: string;
  fragile?: boolean;
  weightClass?: string;
  sizeBucket?: SizeBucket;
  quantity?: number;
  dimensionsCm?: Dims | null;
  dimensionsSource?: DimensionsSource;
};

/**
 * The same manual ordering, for every item in one capture, with each item's
 * likely pick charged against the box it would go into.
 *
 * A photo of a shelf yields several items at once, and ordering each of them
 * against the untouched cache would offer the same half-empty tote to all of
 * them -- the offline shape of exactly the bug the batched /recommend call
 * exists to avoid. This cannot score, so it does the one thing it honestly
 * can: assume each item takes the box at the top of its own list, and let the
 * next item see that space as spent.
 *
 * Deterministic: items in the order given, boxes in the order given, and the
 * running tally is only ever looked up by id, never iterated.
 */
export function orderForOfflinePicks(
  list: Box[],
  items: OfflineItem[],
  sizeLitres: SizeLitres | undefined = cachedSizeLitres(),
): Box[][] {
  const spent = new Map<string, number>();
  return items.map((item) => {
    // The litres earlier items took, added to each box's fill where the fill
    // is known -- an unknown one stays unknown, as in the engine's batch.
    const charged = list.map((b) => withLitres(b, spent.get(b.id)));
    const ordered = orderForOfflinePick(charged, item.category, item, sizeLitres);
    const top = ordered[0];
    const need = needLitres(item, sizeLitres);
    if (top && need !== undefined) spent.set(top.id, (spent.get(top.id) ?? 0) + need);
    return ordered;
  });
}

// ---------------------------------------------------------------------------
// Photos taken but not yet reviewed
// ---------------------------------------------------------------------------
//
// Identification costs about five seconds per item in the photo. One drill is
// three seconds and nobody minds; a shelf of sixteen things is over a minute,
// and standing in front of it for that is how cataloguing a room turns into
// abandoning it halfway.
//
// So a capture can be parked. The photo is already durable by then, the
// request carries on, and the result waits here. This is deliberately NOT the
// default path: a single item still reaches the review screen in three seconds
// exactly as it did before, because always queueing would add a tap and a wait
// to the case that is already fast.
//
// Separate from the catalog queue above, and they must not be confused. That
// one holds work Homebox has not accepted yet; this one holds work the USER
// has not looked at yet. A capture moves from this list to that one when it is
// filed.

let pending: PendingState = { captures: [], working: "", loaded: false };
const pendingListeners = new Set<() => void>();

function setPending(patch: Partial<PendingState>): void {
  pending = { ...pending, ...patch };
  for (const listener of pendingListeners) listener();
}

function subscribePending(listener: () => void): () => void {
  pendingListeners.add(listener);
  return () => {
    pendingListeners.delete(listener);
  };
}

/** Subscribes a component to the photos waiting to be reviewed. */
export function usePending(): PendingState {
  return useSyncExternalStore(subscribePending, () => pending);
}

function persistPending(captures: PendingCapture[]): void {
  writeJson(PENDING_KEY, { version: 1, captures });
  setPending({ captures });
}

/**
 * Reads the pending list from storage. Called by loadAll, BEFORE the photo GC,
 * which needs to know these photos are spoken for.
 *
 * A capture that was mid-identification when the app was killed comes back as
 * `identifying` and is simply picked up again -- the request was never sent
 * anywhere durable, so repeating it costs one call and loses nothing.
 */
export function loadPending(): boolean {
  let captures: PendingCapture[] = [];
  let readable = true;
  try {
    const raw = readText(PENDING_KEY);
    if (raw !== null) {
      const parsed = JSON.parse(raw) as { captures?: unknown };
      if (Array.isArray(parsed.captures)) {
        captures = parsed.captures as PendingCapture[];
      } else {
        // A value we cannot interpret, which is NOT the same as an empty list.
        readable = false;
      }
    }
  } catch {
    readable = false;
  }
  if (!readable) {
    // Kept, not overwritten -- the next persistPending would clobber it -- and
    // reported, so loadAll knows not to collect photos against a reference set
    // that a failure produced. The list of parked captures may be lost either
    // way; their photos need not be.
    quarantine(PENDING_KEY);
  }
  setPending({ captures, working: "", loaded: true });
  return readable;
}

/**
 * Every file the pending list is still holding on to: each photo, and each
 * LiDAR depth file beside it.
 *
 * The depth file lives in the same captures directory, so the startup GC sees
 * it like any other file and deletes whatever this set does not name. Leaving
 * it out would not fail loudly -- the capture would simply come back from a
 * restart with its measurement gone and the vision guess in its place.
 */
export function pendingPhotoNames(): Set<string> {
  const names = new Set<string>();
  for (const c of pending.captures) {
    if (c.photoName) names.add(c.photoName);
    if (c.depthName) names.add(c.depthName);
  }
  return names;
}

/** Adds a freshly taken photo, and starts identifying it. */
export function addPending(capture: PendingCapture): void {
  if (pending.captures.some((c) => c.captureId === capture.captureId)) return;
  persistPending([...pending.captures, capture]);
  void runIdentification();
}

/** Drops one, at the user's explicit request, and releases its photo. */
export function discardPending(captureId: string): void {
  const entry = pending.captures.find((c) => c.captureId === captureId);
  persistPending(pending.captures.filter((c) => c.captureId !== captureId));
  // Only ours to delete if nothing else still needs it.
  if (entry && !queuePhotoNames().has(entry.photoName)) deleteCapturePhoto(entry.photoName);
  // The depth file needs no such check: nothing but this list ever names it.
  deleteCapturePhoto(entry?.depthName);
}

/**
 * Reads one out to be reviewed, WITHOUT removing it.
 *
 * Not removing it is the point. Between opening a parked capture and filing
 * it, the only copy of the drafts is React state and the only claim on the
 * photo would be a screen -- so if iOS kills the app there, the next launch
 * finds a photo neither list claims, collects it, and the capture is gone.
 * Opening something would make it LESS safe than leaving it alone, which is
 * backwards. It stays claimed until releasePending says otherwise.
 */
export function readPending(captureId: string): PendingCapture | undefined {
  return pending.captures.find((c) => c.captureId === captureId);
}

/**
 * Lets go of a capture that has been dealt with -- filed, queued, or thrown
 * away by the user.
 *
 * The photo is deliberately NOT deleted here: by this point either the catalog
 * queue owns it, or the caller is about to decide. clearCapture is what
 * releases the file.
 *
 * The depth file IS deleted here, because this list is its only owner. It
 * never enters queue.v2 -- only the numbers measured from it travel on, inside
 * the drafts -- so once the capture leaves this list nothing could ever claim
 * it again, and depth maps stay on the phone no longer than review needs them.
 */
export function releasePending(captureId: string): void {
  const entry = pending.captures.find((c) => c.captureId === captureId);
  if (!entry) return;
  persistPending(pending.captures.filter((c) => c.captureId !== captureId));
  deleteCapturePhoto(entry.depthName);
}

/** Puts a failed capture back in the queue to be tried again. */
export function retryPending(captureId: string): void {
  persistPending(
    pending.captures.map((c) =>
      c.captureId === captureId ? { ...c, status: "identifying", error: "" } : c,
    ),
  );
  void runIdentification();
}

/** Every photo the CATALOG queue still needs. */
function queuePhotoNames(): Set<string> {
  const names = new Set<string>();
  for (const e of state.entries) if (e.photoName) names.add(e.photoName);
  return names;
}

/**
 * How many times one photo may fail before it is set aside.
 *
 * `attempts` used to be counted and never read, which meant nothing ever gave
 * up: a photo that fails every time -- too large for the provider, corrupt,
 * whatever -- is chosen first on every run because it is the oldest still
 * identifying, so it blocked every photo behind it permanently.
 */
const maxIdentifyAttempts = 3;

// One at a time. Identification is the expensive call in this app -- money and
// a minute of somebody's wait -- and firing five at once would neither arrive
// sooner nor be cheaper, but would make the failure of any one of them harder
// to report.
let inFlight: Promise<void> | null = null;

/**
 * Identifies the oldest photo still waiting, then the next, until none are.
 *
 * Cheap to call on any trigger, like flushQueue. When a run is already going
 * it returns THAT run's promise rather than a resolved one, so awaiting this
 * always means "the work is done" -- a caller that got an instant resolve
 * while a photo was still being identified would have no way to tell.
 */
export function runIdentification(): Promise<void> {
  if (inFlight) return inFlight;
  inFlight = identifyLoop().finally(() => {
    inFlight = null;
    setPending({ working: "" });
  });
  return inFlight;
}

async function identifyLoop(): Promise<void> {
  // With no server there is nobody to ask, and that is not the photo's
  // failure. Without this, every trigger against an unconfigured app -- a
  // Keychain that could not be read at launch, say -- charged the oldest
  // parked photo an attempt, and three of those set it aside as failed. It
  // waits, uncharged, and connectionSaved runs this again once there is a
  // server to send it to.
  if (!isConfigured(currentConnection())) return;
  for (;;) {
    const next = pending.captures.find((c) => c.status === "identifying");
    if (!next) return;
    setPending({ working: next.captureId });

    const uri = await capturePhotoUri(next.photoName);
    if (uri === null) {
      // The photo is gone -- evicted, or deleted by something that thought
      // it was unreferenced. There is nothing left to identify and no way to
      // get it back, so say so rather than retrying forever.
      updatePending(next.captureId, {
        status: "failed",
        error: "The photo for this capture is no longer on the device.",
      });
      continue;
    }

    try {
      const items = await measureFromDepth(await identify(uri), next.depthName);
      updatePending(next.captureId, { status: "ready", items, error: "" });
    } catch (err) {
      const message = errorMessage(err);
      const attempts = (next.attempts ?? 0) + 1;

      // 422 is not a failure to retry: it is "there is no AI provider" or
      // "nothing here was recognised", and the answer to both is the
      // manual-entry form. Marking it READY with no items is what gets the
      // user there -- the review screen turns an empty list into one blank
      // draft, exactly as the old blocking path did. Treating it as a failure
      // took manual entry away from anyone running AI_PROVIDER=none, which is
      // a configuration this project promises stays fully usable.
      if (err instanceof ApiError && err.status === 422) {
        updatePending(next.captureId, { status: "ready", items: [], error: "", attempts });
        continue;
      }

      if (isRetriable(err) && attempts < maxIdentifyAttempts) {
        // The backend is unreachable, so everything behind this would fail the
        // same way, each at a 60-second timeout. Stop and wait for the next
        // trigger. The reason is recorded so the inbox can say why.
        updatePending(next.captureId, { attempts, error: message });
        return;
      }

      // Either it cannot work, or it has had enough goes. Failing it is what
      // un-dams the list: the loop always takes the OLDEST entry still marked
      // identifying, so one photo that never succeeds would otherwise block
      // every photo taken after it, on every trigger, forever.
      updatePending(next.captureId, { status: "failed", attempts, error: message });
    }
  }
}

/**
 * The model's items, measured from the capture's LiDAR depth file when it has
 * one. Here, before the capture is marked ready, because the depth file is
 * deleted once the capture leaves the pending list. Anything that goes wrong
 * leaves the items as the model described them.
 */
async function measureFromDepth(items: ItemDraft[], depthName: string | undefined): Promise<ItemDraft[]> {
  if (!depthName || items.length === 0) return items;
  const uri = await capturePhotoUri(depthName);
  if (uri === null) return items;
  const path = uri.startsWith("file://") ? decodeURI(uri.slice("file://".length)) : uri;
  return attachLidarDimensions(items, path);
}

/** Patches one capture in place, keeping the rest untouched. */
function updatePending(captureId: string, patch: Partial<PendingCapture>): void {
  persistPending(
    pending.captures.map((c) => (c.captureId === captureId ? { ...c, ...patch } : c)),
  );
}
