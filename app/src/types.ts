// Mirrors the Go JSON contracts in backend/internal/placement/engine.go and
// backend/internal/homebox/types.go. If you change one side, change both in
// the same commit (see CLAUDE.md).

export type SizeBucket = "S" | "M" | "L" | "XL";
export type WeightClass = "light" | "medium" | "heavy";

/** How much effort it takes to reach a box. "" means unrecorded. */
export type Access = "easy" | "normal" | "deep" | "";

/**
 * Where an item sits in the photo, as FRACTIONS of its width and height
 * (0..1), origin top-left.
 *
 * Fractions, never pixels: the photo is downscaled on the way in and again by
 * whatever the model was shown, so a pixel box would be measured against an
 * image nobody else has.
 */
export interface Region {
  x: number;
  y: number;
  w: number;
  h: number;
}

/** A size in centimetres, longest side first once the backend has seen it. */
export interface Dims {
  l: number;
  w: number;
  h: number;
}

/**
 * Where an item's size came from. "lidar" and "manual" are measurements and
 * may rule out a container it will not go into; "vision" is the model's
 * estimate from a photo with no scale in it, and never rules anything out.
 */
export type DimensionsSource = "lidar" | "manual" | "vision";

/**
 * Where a container's fill level came from. Only an observation is a fact
 * that may rule the container out; "estimated" is Boxwright adding up what it
 * filed since, and only lowers a score. "" means unknown.
 */
export type FillSource = "observed" | "lidar" | "estimated" | "";

export interface ItemDraft {
  name: string;
  category: string;
  sizeBucket: SizeBucket;
  fragile: boolean;
  weightClass: WeightClass;
  notes: string;
  confidence: number; // 0 for manual entry
  /** How many identical things this capture represents. Floored at 1 server-side. */
  quantity: number;
  /**
   * True when this does not go inside a container at all -- a lawn mower, a
   * bicycle, a floor lamp.
   *
   * A separate fact from sizeBucket rather than a bucket above XL, because it
   * is a difference of kind: the buckets measure an item against a container,
   * and a mower is not a large tote-load. Such an item is only ever placed in
   * an AREA (see Box.isArea).
   */
  bulky: boolean;
  /**
   * Where this item is in the shared photo, when the model said.
   *
   * Usually absent, and that is fine: everything falls back to the whole
   * photo. A wrong box is worse than none -- it crops away the very object the
   * user is being asked to identify -- so the backend drops any box that does
   * not survive clamping rather than repairing it.
   */
  region?: Region;
  /** The item's size, when anything better than sizeBucket is known. */
  dimensionsCm?: Dims | null;
  dimensionsSource?: DimensionsSource;
}

export interface Box {
  id: string;
  name: string;
  /** The row this box sits in ("California"); derived from the hierarchy. */
  parentId: string;
  area: string;
  access: Access;
  /**
   * Tri-state, mirroring *bool in Go. `null` means nobody has recorded it,
   * which is the normal state of a fresh inventory and is NOT the same as
   * false -- an unrecorded flag never excludes a box, it only earns a caveat
   * in `reasons`.
   */
  heavySafe: boolean | null;
  fragileSafe: boolean | null;
  /**
   * What the user recorded about this container. Every one may be absent,
   * which means unknown -- never zero -- and unknown never rules a box out.
   * Optional because a box cached before the backend sent them has none.
   */
  containerType?: string;
  capacityL?: number | null;
  interiorCm?: Dims | null;
  /** 0-100, meaningful only when fillSource is set; an estimate may pass 100. */
  fillPct?: number | null;
  fillSource?: FillSource;
  /** When the fill was last OBSERVED (RFC3339); estimates do not move it. */
  fillCheckedAt?: string;
  /**
   * For clients that predate litres: derived from the above, 0 and 0 when the
   * capacity is unknown. The backend's engine never reads them.
   */
  capacityUnits: number;
  usedUnits: number;
  /**
   * How many things Homebox lists inside, counting quantity -- a fact, unlike
   * usedUnits. Absent from a box cached before the backend sent it.
   */
  itemCount?: number;
  categories: Record<string, number>;
  gridX: number;
  gridY: number;
  /**
   * True for a location that holds OTHER locations -- a garage, a storage
   * unit. Somewhere a lawn mower stands, rather than somewhere a box goes.
   *
   * Offered only for bulky items, and never for anything that fits in a
   * container, so an ordinary drill can never be filed "in the Garage".
   */
  isArea: boolean;
}

export interface Candidate {
  box: Box;
  score: number;
  reasons: string[];
}

export interface NewContainerSuggestion {
  sizeBucket: SizeBucket;
  access: Access;
  label: string;
  reason: string;
  /**
   * One of the user's own container types, when one suits the item. Absent
   * when they have recorded none; the suggestion is then the size bucket.
   */
  containerType?: string;
  capacityL?: number;
  interiorCm?: Dims;
}

/** A kind of container the user has recorded, derived from the containers. */
export interface ContainerType {
  name: string;
  capacityL: number;
  interiorCm: Dims | null;
  /** How many containers carry it. */
  count: number;
}

/**
 * What the client sends back to accept a new-container suggestion. sizeBucket
 * and access are echoed from the suggestion so the created box is annotated;
 * without them it comes back with no capacity recorded and cannot be scored.
 */
export interface NewContainerRequest {
  label: string;
  parentId: string;
  entityTypeId?: string;
  sizeBucket: SizeBucket;
  access: Access;
  /**
   * Echoed from the suggestion when it named one of the user's own container
   * types, so the container is created already knowing its size.
   */
  containerType?: string;
  capacityL?: number;
  interiorCm?: Dims;
}

/**
 * How full a container was seen to be, WITH the item going into it inside --
 * "how full is it now?" answered at the container, or a LiDAR photo of it.
 * A fact: it replaces Boxwright's estimate, unless a newer one is recorded.
 */
export interface FillObservation {
  pct: number;
  source: "observed" | "lidar";
  /** RFC3339, when it was seen. */
  at: string;
}

/** One thing recognised in a photo, and the box it is going into. */
export interface CatalogEntry {
  item: ItemDraft;
  /**
   * This entry's idempotency key, minted ONCE when the entry was built and
   * carried with it from then on -- never recomputed at send time.
   *
   * That distinction is the whole point. After a partial failure the queue
   * re-sends the same capture with a SUBSET of its entries, so a key derived
   * from position would match the resent entry against a different item that
   * already landed, report it as a duplicate, and lose it.
   */
  entryId?: string;
  boxId?: string;
  entityTypeId?: string;
  newContainer?: NewContainerRequest;
  /** How full the destination was seen to be with this item in it, if asked. */
  fillAfter?: FillObservation;
}

/**
 * One photo, N entities. A capture is filed in a single request so the photo
 * crosses the wire once, and so the offline queue holds one self-contained
 * unit of work per capture rather than one per recognised item.
 */
export interface CatalogRequest {
  entries: CatalogEntry[];
  /** Client-generated, so a retry after a lost response is not a second item. */
  captureId?: string;
  /**
   * When the photo was taken (RFC3339). A fill observed after it already
   * includes these items, so the server adds nothing to it.
   */
  capturedAt?: string;
}

/**
 * What became of ONE entry. The entity landing in Homebox is the durable
 * outcome, so metadata and photo failures are reported rather than failing it.
 */
export interface CatalogEntryResult {
  /**
   * True when this entry was already filed by an earlier request carrying the
   * same entryId, so nothing was created now. The flags below then describe
   * that first attempt, which the server did not observe.
   */
  deduped?: boolean;
  /**
   * Optional because a failed entry has no entity. Read it defensively: an
   * `error` and a zero-valued entity are the same news.
   */
  entity?: { id: string; name: string };
  fieldsWritten: boolean;
  fieldsError?: string;
  photoUploaded: boolean;
  photoError?: string;
  /**
   * The item landed but its container's fill was not updated. Not retried:
   * the container reads roomier than it is until someone checks it.
   */
  fillError?: string;
  /** Why this entry did not land. Empty or absent means it did. */
  error?: string;
}

/**
 * PUT /api/v1/containers: what the user records about one or more containers
 * at once. Every field is optional, and only what is present is written.
 */
export interface ContainerSet {
  /** "" clears it. */
  containerType?: string;
  capacityL?: number;
  interiorCm?: Dims;
  access?: Access;
  heavySafe?: boolean;
  fragileSafe?: boolean;
  fill?: FillObservation;
}

export interface PutContainersRequest {
  ids: string[];
  set: ContainerSet;
}

/** results[i] is ids[i]; one container failing does not undo the others. */
export interface ContainerResult {
  id: string;
  /** The container as the index now holds it, when it was written. */
  box?: Box;
  error?: string;
}

export interface PutContainersResponse {
  results: ContainerResult[];
}

/**
 * results[i] corresponds to entries[i]. One entry failing neither rolls back
 * nor hides the others, so the client must read every result rather than the
 * status code alone.
 */
export interface CatalogResponse {
  results: CatalogEntryResult[];
}

/** A photo can hold several things; the model returns one draft per thing. */
export interface IdentifyResponse {
  items: ItemDraft[];
}

/**
 * The answer when nothing fits AND a new container would not help: today, a
 * bulky item with no area to stand it in. Distinct from newContainer because
 * the two ask the user for completely different things.
 */
export interface NoPlaceForItem {
  reason: string;
}

export interface Recommendation {
  candidates: Candidate[];
  newContainer?: NewContainerSuggestion;
  /** At most one of newContainer and noPlace is ever set. */
  noPlace?: NoPlaceForItem;
}

export interface RecommendRequest {
  items: ItemDraft[];
}

/**
 * recommendations[i] corresponds to items[i], same order, same length.
 *
 * Every item from one photo is scored in a single call precisely so they can
 * see each other: the second item competes for the capacity the first just
 * consumed. Scoring them one at a time would recommend the same nearly-full
 * tote to all of them.
 */
export interface RecommendResponse {
  recommendations: Recommendation[];
  stale: boolean;
}

export interface BoxesResponse {
  boxes: Box[];
  stale: boolean;
  /** Absent from a backend that predates container types. */
  containerTypes?: ContainerType[];
  /** Litres an item of each size bucket is taken to occupy, when unmeasured. */
  sizeLitres?: Record<SizeBucket, number>;
}

/**
 * Homebox entity type. Creating a location requires the id of one whose
 * isLocation is true: with entityTypeId omitted the server auto-resolves a
 * non-location type and the "new box" is silently created as an item.
 */
export interface EntityType {
  id: string;
  name: string;
  isLocation: boolean;
}

export interface EntityTypesResponse {
  entityTypes: EntityType[];
}

/**
 * One location in the user's Homebox, and whether they have said Boxwright may
 * file things into it. Mirrors GET /api/v1/locations.
 *
 * EVERY location appears here, whatever its shape. `hasChildren` and
 * `itemCount` are context for the person choosing -- never a filter. A
 * location that holds other locations can be a perfectly good place to put
 * things ("Garage", "Shelf in Pantry"), and deciding otherwise on the user's
 * behalf is exactly the mistake this endpoint exists to undo.
 */
export interface LocationNode {
  id: string;
  name: string;
  parentId?: string;
  parentName?: string;
  /** "Garage > California > Fresno"; already ordered parents-before-children. */
  path: string;
  depth: number;
  itemCount: number;
  hasChildren: boolean;
  eligible: boolean;
}

export interface LocationsResponse {
  locations: LocationNode[];
  eligibleCount: number;
}

/** One row of a PUT /api/v1/locations body. */
export interface LocationChoice {
  id: string;
  eligible: boolean;
}

/**
 * What became of one selection. Choosing eighty locations is eighty writes to
 * Homebox, so partial failure is normal and is reported per location rather
 * than by the status code.
 */
export interface LocationResult {
  id: string;
  eligible: boolean;
  error?: string;
}

export interface LocationsWriteResponse {
  results: LocationResult[];
}

/** What the one-time migration did. `marked` counts successes only. */
export interface AdoptResponse {
  scanned: number;
  marked: number;
  adopted: { id: string; name: string; error?: string }[];
}

/**
 * One category the user may file under. Mirrors GET /api/v1/categories.
 *
 * `key` is what travels as ItemDraft.category and what the engine compares
 * against a box's counted tags; `label` is display only and is the user's own
 * spelling of their tag. `canonical` says the key came from our seed list
 * rather than from their Homebox -- it is a provenance note for the UI, never
 * a filter: an uncanonical category is the normal case, not a lesser one.
 */
export interface CategoryOption {
  key: string;
  label: string;
  /** True when at least one item in Homebox already carries this tag. */
  inUse: boolean;
  itemCount: number;
  canonical: boolean;
}

export interface CategoriesResponse {
  categories: CategoryOption[];
}

// ---------------------------------------------------------------------------
// App-only types below. Nothing here crosses the wire as a Go contract; these
// describe what the app writes to its own disk, and their shape is ours alone.
// ---------------------------------------------------------------------------

/** One item within a queued capture: what it is, and where it is going. */
export interface QueuedEntry {
  item: ItemDraft;
  /**
   * Minted when the entry was built and persisted with it, so a retry over bad
   * signal is recognised as the same item rather than filed a second time.
   *
   * Optional because entries queued before this existed do not have one. They
   * still file; they are simply not protected, which is the right way round --
   * refusing them would lose captures to prevent duplicates.
   */
  entryId?: string;
  boxId?: string;
  newContainer?: NewContainerRequest;
  /** Where it is going, for showing the user what is waiting. */
  boxName: string;
  /** How full the container was seen to be with this in it, when asked. */
  fillAfter?: FillObservation;
}

/**
 * One capture waiting to be filed, held on disk until Homebox has it.
 *
 * Everything /catalog needs is here, because /catalog is a single multipart
 * request: a queue entry is one self-contained unit of work rather than a
 * chain of dependent ones that would have to be ordered and reconciled.
 *
 * The N items recognised in one photo stay together in ONE capture rather than
 * becoming N queue entries, because they share a single photo file. Split
 * apart, the first entry to flush would delete the photo out from under the
 * rest; kept together there is exactly one moment at which the photo is no
 * longer needed. A partial flush rewrites `entries` down to what still has to
 * be filed, so nothing is ever sent twice and nothing is dropped.
 */
export interface QueuedCapture {
  /** Client-generated; makes a retry after a lost response not a second item. */
  captureId: string;
  queuedAt: number;
  entries: QueuedEntry[];
  /**
   * File NAME within the captures directory, never an absolute uri: the
   * document directory's path changes across reinstalls on iOS, so a stored
   * uri can dangle while the file is still there.
   */
  photoName?: string;
  attempts: number;
  /** Why the most recent attempt failed; "" before the first attempt. */
  lastError: string;
}

/**
 * A photo that has been taken but not yet reviewed.
 *
 * Identification takes about five seconds per item in the photo -- three
 * seconds for a single drill, over a minute for a shelf of sixteen things --
 * and standing still for that is the difference between cataloguing a room and
 * giving up halfway. So a capture can be PARKED: the photo is already durable,
 * the identification carries on, and the answer waits here until somebody sits
 * down to review it.
 */
export interface PendingCapture {
  captureId: string;
  queuedAt: number;
  /** File NAME in the captures directory, never a uri. See QueuedCapture. */
  photoName: string;
  /** LiDAR `.depth` file NAME beside the photo; owned by this list alone, never queued. */
  depthName?: string;
  /**
   * `identifying` while the request is out or waiting to be made, `ready` once
   * items are attached, `failed` when the request was refused in a way that
   * repeating it unchanged will not fix.
   */
  status: "identifying" | "ready" | "failed";
  /** What the model found. Present exactly when status is `ready`. */
  items?: ItemDraft[];
  /** Why it failed, in words that can go on screen. */
  error?: string;
  attempts: number;
}

/** The photos taken but not yet reviewed, and whether one is being worked on. */
export interface PendingState {
  captures: PendingCapture[];
  /** captureId currently being identified, or "" when nothing is in flight. */
  working: string;
  /** False until the on-disk list has been read, so "0 waiting" is not a lie. */
  loaded: boolean;
}

/** What the app can still show and do with the backend unreachable. */
export interface QueueState {
  entries: QueuedCapture[];
  flushing: boolean;
  /** Why the last flush stopped short; "" when it drained or never ran. */
  lastError: string;
  /** False until the on-disk queue has been read, so "0 pending" is not a lie. */
  loaded: boolean;
}

/** GET /api/v1/status; see backend/internal/api/status.go. */
export interface StatusResponse {
  /** The backend build, e.g. "0.1.0"; "dev" for an unstamped build. */
  version: string;
  /** The /api/v1 contract generation; compared against SUPPORTED_API_VERSION. */
  apiVersion: number;
  /** False when every item has to be entered by hand (AI_PROVIDER=none). */
  identification: boolean;
  aiProvider: string;
  /** Whether this backend accepts a Homebox URL and token from the app. */
  clientHomebox: boolean;
  homebox: { ok: boolean; error?: string };
  /**
   * The Homebox web UI theme of the user these credentials belong to
   * ("homebox", "forest", ...). Absent when it could not be read, or from a
   * backend that predates it; the app then keeps its own colours.
   */
  homeboxTheme?: string;
}
