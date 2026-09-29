import { useEffect, useRef, useState } from "react";
import {
  ActivityIndicator,
  Alert,
  AppState,
  Image,
  Linking,
  Modal,
  Pressable,
  ScrollView,
  StatusBar,
  StyleSheet,
  Text,
  TextInput,
  View,
} from "react-native";
import Clipboard from "@react-native-clipboard/clipboard";
import { launchCamera, launchImageLibrary } from "react-native-image-picker";
import type { ImagePickerResponse } from "react-native-image-picker";

import {
  adoptLocations,
  catalog,
  entryError,
  errorMessage,
  identify,
  isRetriable,
  landed,
  boxes as fetchBoxList,
  locations as fetchLocations,
  recommend,
  setLocations,
} from "./src/api";
import { ConnectionScreen } from "./src/ConnectionScreen";
import { isConfigured, loadConnection, useConnection } from "./src/connection";
import { coverCrop, cropRect } from "./src/crop";
import type { CropRect } from "./src/crop";
import { FALLBACK_CATEGORIES, categoryKeyFor, categoryLabel, isKnownCategory, normalizeCategory, pickerOptions, resolveTypedCategory } from "./src/categories";
import {
  addPending,
  cachedBoxes,
  cachedCategories,
  discard,
  discardPending,
  enqueue,
  flushQueue,
  loadAll,
  newCaptureId,
  orderForOfflinePicks,
  refreshBoxCache,
  refreshCategoryCache,
  retryPending,
  runIdentification,
  readPending,
  releasePending,
  setQueuedFill,
  toCatalogEntry,
  usePending,
  useQueue,
} from "./src/offline";
import {
  capturePhotoUri,
  deleteCapturePhoto,
  discardTempFiles,
  isFileUri,
  persistCaptureDepth,
  persistCapturePhoto,
  storedPhotoUri,
} from "./src/photo";
import type { PersistedPhoto } from "./src/photo";
import { fillSummary, formatDims, normalizeDims, parseCapacity, parseDims, wantsFillCheck } from "./src/capacity";
import { DepthCamera, DepthKit, callback } from "boxwright-depth";
import type { DepthCameraMode, DepthCameraRef, DepthCapture, DepthStatus } from "boxwright-depth";
import { FILL_GUIDE, measureFillFromFile } from "./src/lidar";
import { setContainers } from "./src/api";
import type {
  Access,
  Box,
  CategoryOption,
  ContainerSet,
  ContainerType,
  Dims,
  FillObservation,
  ItemDraft,
  LocationNode,
  NewContainerSuggestion,
  PendingCapture,
  QueuedCapture,
  QueuedEntry,
  Recommendation,
  SizeBucket,
} from "./src/types";

type Step = "capture" | "connection" | "setup" | "waiting" | "inbox" | "review" | "recommend" | "done";

/** A category the offered vocabulary does not contain yet, and who wrote it. */
type Proposed = { key: string; label: string; byModel: boolean };

/**
 * One thing recognised in the photo (or added by hand), with the editing state
 * that belongs to it alone.
 *
 * `id` exists because nothing about an item is a stable key: two drafts can
 * share a name, and a name is empty while it is being typed.
 */
type Draft = { id: string; item: ItemDraft; proposed: Proposed | null };

/** One item's part of the capture photo, drawn as a clipped view of it. */
type Thumbnail = { uri: string; crop: CropRect; photo: { width: number; height: number } };

/** The per-item thumbnail's side, in points. coverCrop fills exactly this. */
const ITEM_THUMB_SIDE = 56;

/**
 * Where one item is going. A box that exists, or one about to be created.
 *
 * `name` is what the user is shown and what the queue records, so that a
 * pending capture can say where it is headed without a box list to look it up
 * in.
 */
type Destination =
  | { kind: "box"; boxId: string; name: string }
  | {
      kind: "new";
      name: string;
      sizeBucket: SizeBucket;
      access: Access;
      parentId: string;
      /** One of the user's own container types, when the suggestion named one. */
      containerType?: string;
      capacityL?: number;
      interiorCm?: Dims;
    };

/** An item the server refused, kept so the user can be told which and why. */
type Failure = { entry: QueuedEntry; error: string };

/**
 * A manual capture starts with NO category rather than "other".
 *
 * "other" as a default is a tag invented on the user's behalf every time
 * somebody taps past the field, and it lands in Homebox looking like a
 * deliberate answer while contributing nothing to any box's affinity. One tap
 * on a chip is cheap; an inventory seeded with "Other" is not.
 */
const emptyDraft: ItemDraft = {
  name: "",
  category: "",
  sizeBucket: "M",
  fragile: false,
  bulky: false,
  weightClass: "medium",
  notes: "",
  confidence: 0,
  quantity: 1,
};

let draftSeq = 0;
function newDraftId(): string {
  draftSeq += 1;
  return `draft-${draftSeq}`;
}

function blankDraft(): Draft {
  return { id: newDraftId(), item: { ...emptyDraft }, proposed: null };
}

/**
 * Takes an identified item as the model gave it, including a category we have
 * never seen.
 *
 * The model is shown the live vocabulary and told it may propose something
 * new, so a category outside the list is an answer, not an error. Only the
 * shape is normalised; the model's own wording is kept so the chip reads the
 * way it wrote it and the user is asked to confirm a tag before it is created,
 * rather than finding it afterwards in Homebox.
 */
function draftFromIdentified(identified: ItemDraft, vocabulary: readonly CategoryOption[]): Draft {
  const key = categoryKeyFor(identified.category);
  // Quantity is shown in a stepper and multiplied into capacity, so a model
  // that answers 0 or omits it would put a "0 ×" on screen. Floor it here
  // rather than trusting every provider to have been floored server-side.
  const quantity = Number.isFinite(identified.quantity) ? Math.floor(identified.quantity) : 1;
  return {
    id: newDraftId(),
    item: { ...identified, category: key, quantity: Math.max(1, quantity) },
    proposed:
      key !== "" && !isKnownCategory(vocabulary, key)
        ? { key, label: identified.category.trim(), byModel: true }
        : null,
  };
}

/** Turns the access bucket into something readable in a sentence. */
function accessPhrase(access: NewContainerSuggestion["access"]): string {
  switch (access) {
    case "easy":
      return "easy to get to";
    case "deep":
      return "out of the way is fine";
    default:
      return "convenient";
  }
}

function ago(ts: number): string {
  const minutes = Math.max(0, Math.round((Date.now() - ts) / 60_000));
  if (minutes < 1) return "just now";
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.round(minutes / 60);
  return hours < 24 ? `${hours}h ago` : `${Math.round(hours / 24)}d ago`;
}

function plural(n: number, one: string, many: string): string {
  return n === 1 ? one : many;
}

/** "2 ready to review · 3 still working" -- whichever halves are non-zero. */
function pendingSummary(captures: PendingCapture[]): string {
  const ready = captures.filter((c) => c.status === "ready").length;
  const failed = captures.filter((c) => c.status === "failed").length;
  const working = captures.length - ready - failed;
  const parts: string[] = [];
  if (ready > 0) parts.push(`${ready} ready to review`);
  if (working > 0) parts.push(`${working} still working`);
  if (failed > 0) parts.push(`${failed} failed`);
  return parts.join(" · ");
}

/** "3 × " when it matters, "" when it does not. */
function quantityPrefix(item: ItemDraft | undefined): string {
  return (item?.quantity ?? 1) > 1 ? `${item?.quantity} × ` : "";
}

function itemName(item: ItemDraft | undefined): string {
  return item?.name?.trim() || "Untitled";
}

/** The destination for an accepted new-container suggestion. */
function newContainerDestination(suggestion: NewContainerSuggestion, rec?: Recommendation): Destination {
  return {
    kind: "new",
    name: suggestion.label,
    sizeBucket: suggestion.sizeBucket,
    access: suggestion.access,
    // Put the new container alongside the nearest existing candidate, which is
    // what a person would do. With no candidates it lands at the top level and
    // can be moved in Homebox.
    parentId: rec?.candidates?.[0]?.box.parentId ?? "",
    containerType: suggestion.containerType,
    capacityL: suggestion.capacityL,
    interiorCm: suggestion.interiorCm,
  };
}

/** The destination the user gets for free: the best answer for this item. */
function topChoice(rec: Recommendation | undefined, picks: Box[] | undefined): Destination | null {
  const box = rec?.candidates?.[0]?.box ?? picks?.[0];
  if (box) return { kind: "box", boxId: box.id, name: box.name };
  return rec?.newContainer ? newContainerDestination(rec.newContainer, rec) : null;
}

function sameDestination(a: Destination | undefined, b: Destination): boolean {
  if (!a || a.kind !== b.kind) return false;
  return a.kind === "box" && b.kind === "box" ? a.boxId === b.boxId : true;
}

export default function App() {
  const [step, setStep] = useState<Step>("capture");
  const [busy, setBusy] = useState(false);
  const [captureId, setCaptureId] = useState("");
  const [photo, setPhoto] = useState<PersistedPhoto | null>(null);
  /**
   * Every recognised thing from this photo. One photo is one capture but many
   * items, each filed as its own Homebox entity.
   */
  const [drafts, setDrafts] = useState<Draft[]>([]);
  /** Which item's editor is open. Meaningless (and unused) for a single item. */
  const [expandedId, setExpandedId] = useState("");
  /**
   * The vocabulary to offer. Starts as the seed list so the picker is never
   * empty, then gives way to the cache, then to a live fetch.
   */
  const [categories, setCategories] = useState<CategoryOption[]>(() => [...FALLBACK_CATEGORIES]);
  /** False while `categories` is still only our seeds -- worth admitting to. */
  const [categoriesLive, setCategoriesLive] = useState(false);
  /** recommendations[i] belongs to drafts[i]; empty when we fell back offline. */
  const [recs, setRecs] = useState<Recommendation[]>([]);
  const [stale, setStale] = useState(false);
  /** Set instead of `recs` when the backend could not be reached; one list per draft. */
  const [offlinePicks, setOfflinePicks] = useState<Box[][] | null>(null);
  /** Where each item is going, by draft id. Seeded with the top suggestions. */
  const [choices, setChoices] = useState<Record<string, Destination>>({});
  /** Which item is showing its alternatives. */
  const [openDestId, setOpenDestId] = useState("");
  const [filed, setFiled] = useState<QueuedEntry[]>([]);
  const [failures, setFailures] = useState<Failure[]>([]);
  const [queued, setQueued] = useState<QueuedEntry[]>([]);
  /**
   * True once the offline queue holds this capture's photo. From that moment
   * the file belongs to the queue and this screen must never delete it: the
   * photo is shared by every entry still waiting, and taking it away would
   * strip the picture from all of them.
   */
  /**
   * The capture photo's size in pixels, once measured -- all a per-item crop
   * needs, because a crop is DRAWN (a clipped view of the one photo, see
   * src/crop.ts) rather than written to a file. Null until measured, and a
   * card without it simply shows no thumbnail, as it always could.
   */
  const [photoSize, setPhotoSize] = useState<{ width: number; height: number } | null>(null);
  /**
   * Which capture the screen is on, bumped whenever one ends.
   *
   * /identify, /recommend and /catalog all take up to 60 seconds and none of
   * them is cancelled when the user leaves. Without this, a request that
   * settles after its capture was discarded writes recommendations, choices
   * and a step change into a screen that has moved on -- and worse, queues a
   * capture whose photo has just been deleted. Every continuation checks the
   * generation it started in before touching state.
   */
  const generation = useRef(0);
  /**
   * True from the moment a photo source is tapped until its capture is under
   * way. Not the `busy` state: that is only true after the next render, and
   * the gap is exactly long enough to tap a second source.
   */
  const picking = useRef(false);
  /** Whether Boxwright's own (LiDAR) camera is open. */
  const [depthCamera, setDepthCamera] = useState(false);
  /** The depth file of the photo being captured, handed to beginCapture. */
  const depthPath = useRef<string | undefined>(undefined);
  const [photoQueued, setPhotoQueued] = useState(false);
  /**
   * The same fact as photoQueued, readable at the moment somebody acts on it.
   *
   * An Alert's button captures state by VALUE when the alert is opened. The
   * discard confirmation can sit on screen for as long as the user leaves it
   * there, and a flush finishing underneath it hands the photo to the queue in
   * the meantime -- at which point the captured `false` deletes a photo that
   * several pending entries still point at. A ref is read when the button is
   * pressed, which is the only moment the answer matters.
   */
  const photoQueuedRef = useRef(false);
  const [photoWarning, setPhotoWarning] = useState("");
  const [showQueue, setShowQueue] = useState(false);
  /**
   * How many locations the engine can currently choose between.
   *
   * Read off the box cache rather than fetched separately, so the capture
   * screen costs no extra request. Zero means the user has not told Boxwright
   * where it may put things -- a brand new install always starts here -- and
   * without saying so the app would go on suggesting a new container for every
   * single item and never explain why.
   */
  const [placeCount, setPlaceCount] = useState(() => cachedBoxes().length);
  const queue = useQueue();
  const pending = usePending();
  const server = useConnection();
  /**
   * A fresh install has nowhere to send anything, so the connection screen
   * comes first. Never decided before the Keychain has been read: a returning
   * user would otherwise see the first-run screen flash past on every launch.
   */
  const needsServer = server.loaded && !isConfigured(server.connection);
  /**
   * Which parked capture this screen is waiting on, or "" when it is not
   * waiting on one. Set while `step` is "waiting"; cleared the moment the user
   * parks it, which is what turns waiting into walking away.
   */
  const [watching, setWatching] = useState("");

  useEffect(() => {
    loadAll();
    // Off disk first and synchronously: the picker has to be usable in a
    // storage unit with no signal, where the fetch below will never land.
    const cached = cachedCategories();
    if (cached.length > 0) {
      setCategories(cached);
      setCategoriesLive(true);
    }

    const refresh = () => {
      // Identification only runs while the app is open -- React Native has no
      // reliable background execution -- so a capture parked on the way out of
      // a storage unit resumes here, on the way back in.
      void runIdentification();
      void refreshBoxCache().then(() => setPlaceCount(cachedBoxes().length));
      void refreshCategoryCache().then((list) => {
        if (!list) return;
        setCategories(list);
        setCategoriesLive(true);
      });
    };

    // Nothing goes over the network until the saved connection is known.
    // Flushing or identifying against no server first would charge every
    // waiting capture a failed attempt on each launch -- and three of those
    // set a parked photo aside for good.
    let unmounted = false;
    let sub: { remove: () => void } | null = null;
    void loadConnection().then(() => {
      if (unmounted) return;
      void flushQueue();
      refresh();
      // Coming back to the foreground is the moment a walk out of the storage
      // unit becomes visible to us, and the cheapest reliable flush trigger
      // there is. There is no connectivity listener to consult: a phone with
      // bars still may not be able to reach this particular backend.
      sub = AppState.addEventListener("change", (next) => {
        if (next !== "active") return;
        void flushQueue();
        refresh();
      });
    });
    return () => {
      unmounted = true;
      sub?.remove();
    };
  }, []);

  /**
   * After the connection screen. A move to another inventory has already
   * dropped the old one's caches (offline.switchConnection), so the screen's
   * own copies go too, and everything is fetched again from the new server.
   */
  function connectionSaved(moved: boolean): void {
    if (moved) {
      setCategories([...FALLBACK_CATEGORIES]);
      setCategoriesLive(false);
      setPlaceCount(cachedBoxes().length);
    }
    void refreshBoxCache().then(() => setPlaceCount(cachedBoxes().length));
    void refreshCategoryCache().then((list) => {
      if (!list) return;
      setCategories(list);
      setCategoriesLive(true);
    });
    void flushQueue();
    void runIdentification();
    setStep("capture");
  }

  /**
   * The picture of one item, when the model said where it is and the photo
   * has been measured. Undefined -- the common case -- shows no thumbnail;
   * the whole photo is above the list either way.
   */
  function thumbnailFor(region: ItemDraft["region"]): Thumbnail | undefined {
    if (!photo || !photoSize) return undefined;
    const crop = cropRect(region, photoSize.width, photoSize.height);
    return crop ? { uri: photo.uri, crop, photo: photoSize } : undefined;
  }

  /**
   * Measures the capture photo, once per photo, for the per-item crops.
   *
   * Cancellable, because the user may have moved on to another capture by the
   * time the measurement lands -- and a size from the previous photo would
   * crop the next one in the wrong places.
   */
  useEffect(() => {
    const uri = photo?.uri;
    setPhotoSize(null);
    if (!uri) return;
    let cancelled = false;
    Image.getSize(
      uri,
      (width, height) => {
        if (!cancelled) setPhotoSize({ width, height });
      },
      // Unmeasurable is not worth reporting: every card still has the text
      // that names it, and the whole photo is above the list.
      () => {},
    );
    return () => {
      cancelled = true;
    };
  }, [photo?.uri]);

  /**
   * Runs one photo source and starts a capture from whatever it returns.
   *
   * The guard is the point. `busy` is React state, so it is not true until the
   * next render, and the clipboard read in particular takes real time on a
   * large image while the screen stays fully interactive -- long enough for
   * somebody who sees nothing happening to tap another source. That would
   * start a second capture over the top of the first, and because identify()
   * had no ownership check the review screen could end up showing the second
   * photo under the first photo's item names. A ref is true immediately.
   *
   * Every native call is wrapped, because all three can reject rather than
   * cancel -- a photo iOS has offloaded to iCloud and cannot fetch, a denied
   * camera, an unreadable clipboard. Unhandled, those are a button that
   * silently does nothing, with no log at all in a release build.
   */
  async function fromSource(
    failureTitle: string,
    pick: () => Promise<string | null>,
  ): Promise<void> {
    if (picking.current) return;
    picking.current = true;
    setBusy(true);
    try {
      const chosen = await pick();
      // Cancelled, which is not a failure and needs no telling.
      if (!chosen) return;
      await beginCapture(chosen);
    } catch (err) {
      Alert.alert(failureTitle, errorMessage(err));
    } finally {
      picking.current = false;
      // beginCapture clears this on the way to the review screen; clearing it
      // again is harmless and covers the cancel and failure paths.
      setBusy(false);
    }
  }

  /**
   * Takes the screen to review the moment the capture it is waiting on is
   * ready -- so a single item still lands on the form in about three seconds,
   * exactly as it did before any of this was queued.
   *
   * Nothing happens if the user has parked it: `watching` is cleared on the
   * way out, and the identification simply carries on into the list.
   */
  useEffect(() => {
    if (step !== "waiting" || watching === "") return;
    const capture = pending.captures.find((c) => c.captureId === watching);
    // Gone from the list entirely means it was discarded from elsewhere.
    if (!capture) {
      setWatching("");
      setStep("capture");
      return;
    }
    if (capture.status === "ready") {
      openPending(capture);
      return;
    }
    if (capture.status === "failed") {
      // Left in the list rather than dropped: the photo is still good, and the
      // user can retry it or throw it away from the inbox.
      setWatching("");
      setStep("inbox");
    }
    // openPending only reads refs and setters, so it needs no dependency here.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [pending.captures, step, watching]);

  /**
   * Turns a picker result into a photo uri, or null after telling the user why
   * there is none.
   *
   * The camera asks for permission itself (iOS shows NSCameraUsageDescription
   * the first time), so there is nothing to request up front -- only a refusal
   * to explain, with a way to the setting that undoes it.
   */
  function pickedUri(result: ImagePickerResponse): string | null {
    if (result.didCancel) return null;
    if (result.errorCode === "permission") {
      Alert.alert("Camera access needed", "Boxwright photographs items to catalog them.", [
        { text: "Not now", style: "cancel" },
        { text: "Open Settings", onPress: () => void Linking.openSettings() },
      ]);
      return null;
    }
    if (result.errorCode === "camera_unavailable") {
      Alert.alert("No camera here", "Choose an existing photo instead.");
      return null;
    }
    if (result.errorCode) throw new Error(result.errorMessage ?? result.errorCode);
    return result.assets?.[0]?.uri ?? null;
  }

  /**
   * No saveToPhotos: filing a drill into Homebox is not a reason to put it in
   * someone's camera roll, and it would need a permission of its own.
   * "compatible" makes the library hand back JPEG rather than HEIC, so even a
   * photo that photo.downscale fails on is one every vision model reads. No
   * size limit here: downscale is the one resize, and a second would encode
   * every photo as JPEG twice.
   */
  const photoPickerOptions = {
    mediaType: "photo",
    quality: 0.7,
    assetRepresentationMode: "compatible",
    includeExtra: false,
  } as const;

  /**
   * Take a photo now. On a phone with LiDAR it is Boxwright's own camera, which
   * keeps the depth alongside the photo so each item can be measured; anywhere
   * else, and whenever the user asks for it, the system camera as before.
   */
  function takePhoto(): Promise<void> {
    if (DepthKit.isSupported) {
      setDepthCamera(true);
      return Promise.resolve();
    }
    return systemCamera();
  }

  function systemCamera(): Promise<void> {
    return fromSource("Could not open the camera", async () => pickedUri(await launchCamera(photoPickerOptions)));
  }

  /** A photo from the depth camera: the same path as any other, with its depth. */
  function depthShot(shot: DepthCapture): Promise<void> {
    setDepthCamera(false);
    depthPath.current = shot.depthPath;
    return fromSource("Could not use that photo", async () => `file://${encodeURI(shot.photoPath)}`);
  }

  /**
   * Use a photo that already exists.
   *
   * Not everything is in front of you when you catalog it: the box may be
   * taped shut and the photo taken a week ago, or the picture may be a product
   * listing saved from a browser. Deliberately no permission request -- the
   * system picker runs outside the app and hands over only the one photo the
   * user chose, so asking for library access would be asking for more than
   * this needs. It is also the only way in on the Simulator, which has no
   * camera.
   */
  function chooseExisting(): Promise<void> {
    return fromSource("Could not use that photo", async () => pickedUri(await launchImageLibrary(photoPickerOptions)));
  }

  /**
   * Use an image on the clipboard.
   *
   * The other half of "a picture that already exists": long-pressing an image
   * on a product page offers Copy as readily as it offers Add to Photos, and
   * without this the copy route is a dead end.
   *
   * The clipboard is read only when this is pressed, never probed in the
   * background -- iOS shows the user a banner when an app reads it, and doing
   * that unasked, on a screen they only opened to photograph something, would
   * be alarming and pointless.
   */
  function pasteImage(): Promise<void> {
    return fromSource("Could not read the clipboard", async () => {
      const image = await Clipboard.getImageJPG();
      if (!image) {
        Alert.alert(
          "No image copied",
          "Copy a picture first — long-press an image in Safari or Photos and choose Copy.",
        );
        return null;
      }
      // A data: uri, not a file; persistCapturePhoto renders it to one. The
      // module wraps its base64 at 64 characters, and a data: URL with line
      // breaks in it does not parse -- so they go before anything reads it.
      return image.replace(/\s+/g, "");
    });
  }

  /**
   * Everything after a photo has come from somewhere: make it durable,
   * identify it, and land on the review screen.
   *
   * One path for all three sources, because everything downstream -- the
   * offline queue, the crop, the upload -- depends on the photo being a
   * downscaled JPEG in the document directory, and that guarantee should hold
   * whatever the user did to produce it.
   */
  async function beginCapture(sourceUri: string): Promise<void> {
    // A new capture supersedes anything still in flight for the last one.
    // `picking` should already make a second call here impossible; this makes
    // the invariant enforced rather than merely documented, so a stale
    // identify can never write its item names over a different photo.
    const mine = ++generation.current;
    setBusy(true);
    const id = newCaptureId();
    setCaptureId(id);

    // Promote the photo out of the picker's cache directory before anything
    // else, so it is already durable if this capture ends up queued.
    let stored: PersistedPhoto;
    try {
      stored = await persistCapturePhoto(id, sourceUri);
    } catch (err) {
      // A file source can still be used where it lies. An empty name means
      // "not durable": the item can still be cataloged right now, but the
      // photo will not survive being queued, and the done screen says so
      // rather than pretending otherwise.
      if (isFileUri(sourceUri)) {
        stored = { name: "", uri: sourceUri, downscaled: false };
        console.warn("could not store the capture photo:", errorMessage(err));
      } else {
        // A pasted image that could not be decoded is not a photo at all.
        // Starting a capture around it would produce a review screen whose
        // picture fails to load and whose upload fails at the end.
        if (mine === generation.current) {
          setBusy(false);
          setCaptureId("");
        }
        Alert.alert("Could not use that image", errorMessage(err));
        return;
      }
    }
    // The depth file travels with the capture only when the capture itself is
    // durable: a capture identified in place has no pending entry to own it,
    // and an unowned file would be collected on the next launch anyway.
    const depthTmp = depthPath.current;
    depthPath.current = undefined;
    let depthName: string | undefined;
    if (depthTmp && stored.name !== "") {
      try {
        depthName = await persistCaptureDepth(id, depthTmp);
      } catch (err) {
        console.warn("could not keep the depth for this capture:", errorMessage(err));
      }
    } else {
      discardTempFiles(depthTmp);
    }
    if (mine !== generation.current) return;
    setPhoto(stored);

    // Identification now happens OUTSIDE this component, against a durable
    // list, so that leaving the screen does not abandon it. What used to be an
    // await is a subscription: this screen watches one capture, and the
    // watching effect below takes it to review when it is ready.
    //
    // A photo whose file could not be stored has no name, so there is nothing
    // durable to park -- it is identified in place, as it always was, and the
    // done screen already warns that its photo will not survive being queued.
    if (stored.name === "") {
      await identifyInPlace(stored.uri, mine);
      return;
    }
    addPending({
      captureId: id,
      queuedAt: Date.now(),
      photoName: stored.name,
      depthName,
      status: "identifying",
      attempts: 0,
    });
    setWatching(id);
    setBusy(false);
    setStep("waiting");
  }

  /**
   * The old blocking path, kept for the one case that cannot use the queue:
   * a photo that could not be written to the document directory.
   */
  async function identifyInPlace(uri: string, mine: number): Promise<void> {
    try {
      const items = await identify(uri);
      if (mine !== generation.current) return;
      setDrafts(items.length > 0 ? items.map((item) => draftFromIdentified(item, categories)) : [blankDraft()]);
    } catch (err) {
      if (mine !== generation.current) return;
      // AI_PROVIDER=none or an AI failure: fall through to manual entry.
      setDrafts([blankDraft()]);
      Alert.alert("Enter details manually", errorMessage(err));
    } finally {
      if (mine === generation.current) {
        setExpandedId("");
        setBusy(false);
        setStep("review");
      }
    }
  }

  /**
   * Opens a capture whose identification has finished.
   *
   * It STAYS in the pending list while it is being reviewed. The drafts on
   * this screen exist only in React state, so a capture removed from the list
   * the moment it is opened would be lost outright if the app were killed
   * mid-review -- and its photo collected on the next launch. clearCapture
   * releases it once it has actually been dealt with.
   */
  function openPending(capture: PendingCapture): void {
    const taken = readPending(capture.captureId);
    if (!taken) return;
    const mine = ++generation.current;
    setCaptureId(taken.captureId);
    setPhoto({ name: taken.photoName, uri: storedPhotoUri(taken.photoName), downscaled: true });
    // Checking the file is there is async on bare React Native, so it is
    // checked after the screen opens: a photo that has gone is dropped from
    // the capture rather than attached to it as a file that fails at upload.
    void capturePhotoUri(taken.photoName).then((uri) => {
      if (uri === null && mine === generation.current) setPhoto(null);
    });
    setPhotoQueued(false);
    photoQueuedRef.current = false;
    const items = taken.items ?? [];
    setDrafts(items.length > 0 ? items.map((item) => draftFromIdentified(item, categories)) : [blankDraft()]);
    setExpandedId("");
    setWatching("");
    setStep("review");
  }

  // -------------------------------------------------------------------------
  // Review
  // -------------------------------------------------------------------------

  function updateDraft(id: string, patch: Partial<ItemDraft>): void {
    setDrafts((current) => current.map((d) => (d.id === id ? { ...d, item: { ...d.item, ...patch } } : d)));
  }

  function chooseCategory(id: string, key: string, label: string): void {
    setDrafts((current) =>
      current.map((d) =>
        d.id === id
          ? {
              ...d,
              item: { ...d.item, category: key },
              // A label only means anything for a key the vocabulary lacks;
              // for an existing category the server's own spelling wins.
              proposed: isKnownCategory(categories, key) ? null : { key, label, byModel: false },
            }
          : d,
      ),
    );
  }

  function addItem(): void {
    const added = blankDraft();
    setDrafts((current) => [...current, added]);
    setExpandedId(added.id);
  }

  /**
   * Removing is two taps, not one.
   *
   * The model does imagine things, so removal has to be within reach -- but a
   * mis-tap here throws away a real thing the user photographed and would have
   * to notice was missing, later, from Homebox. Nothing has been filed yet, so
   * the confirmation costs a moment and can only be exchanged for a loss.
   */
  function confirmRemove(draft: Draft, index: number): void {
    const name = draft.item.name.trim() || `Item ${index + 1}`;
    Alert.alert(
      "Remove this item?",
      `“${name}” will not be cataloged. Everything else in this photo is unaffected.`,
      [
        { text: "Keep it", style: "cancel" },
        {
          text: "Remove",
          style: "destructive",
          onPress: () => setDrafts((current) => current.filter((d) => d.id !== draft.id)),
        },
      ],
    );
  }

  /** The first item that cannot be filed yet, so the user is told which. */
  function firstIncomplete(): { draft: Draft; index: number } | null {
    for (let i = 0; i < drafts.length; i++) {
      const draft = drafts[i];
      if (!draft) continue;
      if (draft.item.name.trim() === "" || draft.item.category === "") return { draft, index: i };
    }
    return null;
  }

  function reviewButtonLabel(): string {
    if (drafts.length === 0) return "Add an item";
    const missing = firstIncomplete();
    if (!missing) return drafts.length === 1 ? "Find a box" : `Find boxes for ${drafts.length} items`;
    const needsName = missing.draft.item.name.trim() === "";
    if (drafts.length === 1) return needsName ? "Name it first" : "Pick a category";
    return needsName
      ? `Name item ${missing.index + 1}`
      : `Pick a category for “${missing.draft.item.name.trim()}”`;
  }

  async function getRecommendation() {
    const missing = firstIncomplete();
    if (missing) {
      // With a list, the offending item may be collapsed and off screen. Open
      // it rather than leaving a dead button and a label nobody can act on.
      setExpandedId(missing.draft.id);
      return;
    }
    if (drafts.length === 0) return;

    setBusy(true);
    const mine = generation.current;
    const items = drafts.map((d) => d.item);
    try {
      const res = await recommend(items);
      if (mine !== generation.current) return;
      // The contract pairs recommendations[i] with items[i]. A short list is a
      // server bug; treat what is missing as "no candidates" rather than
      // shifting somebody else's answer onto this item.
      const list = drafts.map((_, i) => res.recommendations[i] ?? { candidates: [] });
      setRecs(list);
      setStale(res.stale);
      setOfflinePicks(null);
      setChoices(seedChoices(drafts, list, null, choices));
      setOpenDestId("");
      setStep("recommend");
    } catch (err) {
      if (mine !== generation.current) return;
      const cached = cachedBoxes();
      if (isRetriable(err) && cached.length > 0) {
        const lists = orderForOfflinePicks(cached, items);
        setRecs([]);
        setStale(false);
        setOfflinePicks(lists);
        setChoices(seedChoices(drafts, null, lists, choices));
        setOpenDestId("");
        setStep("recommend");
        return;
      }
      Alert.alert(
        "No recommendation",
        isRetriable(err) && cached.length === 0
          ? `${errorMessage(err)}\n\nThere is no saved box list to fall back on yet. Open Boxwright once while the backend is reachable and it will keep one for next time.`
          : errorMessage(err),
      );
    } finally {
      // Only ours to clear. A newer capture may already have set it.
      if (mine === generation.current) setBusy(false);
    }
  }

  // -------------------------------------------------------------------------
  // Recommend
  // -------------------------------------------------------------------------

  /**
   * The destination for each item: the one the user chose, or the engine's
   * best answer for the ones they have not touched.
   *
   * `previous` is why this takes an argument at all. Going back to the item
   * list and forward again re-runs the recommendation, and seeding from
   * scratch would quietly throw away every destination the user had picked by
   * hand -- they would return to the screen, see the engine's suggestions
   * again, and have no way to know their choices had been reverted. An
   * explicit choice outranks a fresh suggestion.
   *
   * Choices for drafts that no longer exist are dropped, so removing an item
   * on the way back does not leave its destination behind.
   */
  function seedChoices(
    list: Draft[],
    recommendations: Recommendation[] | null,
    picks: Box[][] | null,
    previous?: Record<string, Destination>,
  ): Record<string, Destination> {
    const seeded: Record<string, Destination> = {};
    list.forEach((d, i) => {
      const kept = previous?.[d.id];
      if (kept) {
        seeded[d.id] = kept;
        return;
      }
      const top = topChoice(recommendations?.[i], picks?.[i]);
      if (top) seeded[d.id] = top;
    });
    return seeded;
  }

  /**
   * Picks a destination for one item -- or, when the photo held exactly one
   * item, files it there and then.
   *
   * A single item is the common case and it worked in one tap before this
   * screen learned to hold lists. It still does.
   */
  function choose(draft: Draft, destination: Destination): void {
    const next = { ...choices, [draft.id]: destination };
    setChoices(next);
    setOpenDestId("");
    if (drafts.length === 1) {
      const entries = entriesFor(next);
      if (entries) void submit(entries);
    }
  }

  /** Every item with its destination, or null when one is still undecided. */
  /**
   * The entries for this capture, each carrying the idempotency key it will
   * keep for the rest of its life.
   *
   * The key is `<captureId>-<index>` and is computed HERE, once, then stored
   * with the entry -- in React state now and on the offline queue if it has to
   * wait. It is deterministic so that calling this twice for the same drafts
   * produces the same keys, and it is never recomputed at send time: a partial
   * flush re-sends a SUBSET of the entries, at which point index 0 is a
   * different item and a recomputed key would match it against one that
   * already landed. captureId is unique per capture, so the pair is unique.
   */
  function entriesFor(map: Record<string, Destination>): QueuedEntry[] | null {
    const entries: QueuedEntry[] = [];
    for (const [index, draft] of drafts.entries()) {
      const destination = map[draft.id];
      if (!destination) return null;
      entries.push({
        item: draft.item,
        entryId: `${captureId}-${index}`,
        boxId: destination.kind === "box" ? destination.boxId : undefined,
        newContainer:
          destination.kind === "new"
            ? {
                label: destination.name,
                parentId: destination.parentId,
                sizeBucket: destination.sizeBucket,
                access: destination.access,
                containerType: destination.containerType,
                capacityL: destination.capacityL,
                interiorCm: destination.interiorCm,
              }
            : undefined,
        boxName: destination.name,
      });
    }
    return entries;
  }

  async function submit(entries: QueuedEntry[]) {
    // Reached from the offline picker, so the backend was unreachable seconds
    // ago. Queue straight away rather than making someone stand in a storage
    // unit watching a 60s timeout, and let the background flush be the thing
    // that discovers signal came back.
    if (offlinePicks) {
      queueCapture(entries);
      void flushQueue();
      return;
    }

    setBusy(true);
    const mine = generation.current;
    try {
      const res = await catalog(
        // capturedAt is when the items went in: a fill observed later already
        // includes them, so the server adds nothing to it.
        { entries: entries.map(toCatalogEntry), captureId, capturedAt: new Date().toISOString() },
        photo?.uri,
      );
      // The capture this was filing has been discarded or replaced. The items
      // are in Homebox either way -- the request succeeded -- so there is
      // nothing to queue and nothing to report onto a screen that has moved on.
      if (mine !== generation.current) return;
      // 201 means at least one entry landed, never that all of them did, so
      // every result is read before anyone is told this worked.
      const landedEntries: QueuedEntry[] = [];
      const refused: Failure[] = [];
      let photoMisses = 0;
      entries.forEach((entry, i) => {
        const result = res.results[i];
        if (!landed(result)) {
          refused.push({ entry, error: entryError(result) });
          return;
        }
        landedEntries.push(entry);
        if (photo && result && !result.photoUploaded) photoMisses += 1;
      });

      setFiled(landedEntries);
      setFailures(refused);
      // Persist what Homebox refused, do not merely display it. Before this,
      // refused entries lived in React state until the user acted on them, so
      // killing the app on the done screen lost them silently -- a window this
      // multi-item change opened, since a single-item catalog previously
      // either landed or threw. In the queue they survive a restart, show up
      // in the pending banner, and can be retried or discarded deliberately.
      // A permanently refused entry (its box deleted in Homebox) simply fails
      // again with its error visible, which beats vanishing.
      if (refused.length > 0) {
        enqueue({
          captureId: `${captureId}-refused`,
          queuedAt: Date.now(),
          entries: refused.map((f) => f.entry),
          photoName: photo?.name ? photo.name : undefined,
          attempts: 1,
          lastError: refused[0]?.error ?? "Homebox refused this item",
        });
      }
      setQueued([]);
      // The items landed, which is the outcome that matters, but say so
      // plainly if the photo did not follow them.
      setPhotoWarning(photoMissWarning(photoMisses, landedEntries.length, photo !== null));
      setStep("done");
      // The connection is demonstrably up and box contents just changed.
      void flushQueue();
      void refreshBoxCache();
    } catch (err) {
      // Queueing a capture the user has already discarded would re-file items
      // they abandoned, against a photo file that clearCapture has deleted --
      // so the queue would carry a photoName pointing at nothing. The failure
      // is theirs to have caused by leaving; there is nothing to recover.
      if (mine !== generation.current) return;
      if (isRetriable(err)) {
        queueCapture(entries);
        return;
      }
      // Homebox rejected the request itself, so retrying it unchanged would
      // only grow a queue that can never drain. Still never decide to drop it
      // on the user's behalf -- ask.
      Alert.alert("Homebox rejected this", errorMessage(err), [
        { text: "Back", style: "cancel" },
        // Read at press time: the alert can sit here while the capture ends.
        { text: "Keep it queued", onPress: () => {
          if (mine === generation.current) queueCapture(entries);
        } },
      ]);
    } finally {
      if (mine === generation.current) setBusy(false);
    }
  }

  function photoMissWarning(misses: number, total: number, hadPhoto: boolean): string {
    if (!hadPhoto || misses === 0) return "";
    if (misses === total) return "The photo did not upload.";
    return `The photo did not upload for ${misses} of the ${total} items.`;
  }

  /**
   * Hands the whole capture -- every entry and the one photo they share -- to
   * the offline queue as a single unit of work.
   */
  function queueCapture(entries: QueuedEntry[]): void {
    const entry: QueuedCapture = {
      captureId,
      queuedAt: Date.now(),
      entries,
      photoName: photo?.name ? photo.name : undefined,
      attempts: 0,
      lastError: "",
    };
    enqueue(entry);
    setPhotoQueued(true);
    photoQueuedRef.current = true;
    setFiled([]);
    setFailures([]);
    setQueued(entries);
    setPhotoWarning(photo && !photo.name ? "The photo could not be saved and will not be uploaded." : "");
    setStep("done");
  }

  /**
   * Puts the items Homebox refused onto the queue, keeping the ones that
   * landed out of it.
   *
   * Same captureId: this is the same capture and the same photo, minus what is
   * already filed. Re-sending an entry that succeeded would create a second
   * copy, so only the refused ones travel.
   */
  function queueFailures(): void {
    if (failures.length === 0) return;
    const entries = failures.map((f) => f.entry);
    enqueue({
      captureId,
      queuedAt: Date.now(),
      entries,
      photoName: photo?.name ? photo.name : undefined,
      attempts: 0,
      lastError: failures[0]?.error ?? "",
    });
    setPhotoQueued(true);
    photoQueuedRef.current = true;
    setQueued(entries);
    setFailures([]);
    // Deliberately no flush. These entries were refused seconds ago by a
    // backend that is demonstrably up, so sending them straight back would
    // occupy the queue for a 60s round trip to be told the same thing. The
    // next capture, the next foreground, or "Retry now" will try again.
  }

  // -------------------------------------------------------------------------
  // Done
  // -------------------------------------------------------------------------

  function doneHeadline(): string {
    if (filed.length === 0 && failures.length === 0 && queued.length > 0) {
      const only = queued[0];
      return queued.length === 1 && only
        ? `Saved on this phone. ${quantityPrefix(only.item)}“${itemName(only.item)}” will file itself into ${only.boxName} once the backend is reachable.`
        : `Saved on this phone. ${queued.length} items will file themselves once the backend is reachable.`;
    }
    if (failures.length === 0) {
      const only = filed[0];
      return filed.length === 1 && only
        ? `Filed ${quantityPrefix(only.item)}“${itemName(only.item)}” into ${only.boxName}.`
        : `Filed ${filed.length} items.`;
    }
    if (filed.length === 0) {
      return `Nothing was filed. ${failures.length} ${plural(failures.length, "item", "items")} did not reach Homebox.`;
    }
    return `${filed.length} filed, ${failures.length} failed.`;
  }

  /**
   * Throws away a capture the user is part way through reviewing.
   *
   * Behind a confirmation because the photo goes with it and cannot be got
   * back -- the item may already be in the box by now. Nothing here has
   * reached Homebox or the queue, so this is a straight discard rather than
   * anything that needs unpicking.
   */
  /**
   * Leaves the location picker, by either route.
   *
   * The refresh is the point of having one function: a selection just changed
   * which containers exist, and the cache the capture screen counts -- and the
   * offline picker reads -- has to catch up. Backing out instead of pressing
   * Done is not a reason to show a stale number.
   */
  function exitSetup(): void {
    void refreshBoxCache().then(() => setPlaceCount(cachedBoxes().length));
    setStep("capture");
  }

  function confirmAbandon(): void {
    Alert.alert(
      "Discard this capture?",
      drafts.length > 1
        ? `The photo and all ${drafts.length} items found in it will be discarded.`
        : "The photo and what was found in it will be discarded.",
      [
        { text: "Keep editing", style: "cancel" },
        {
          text: "Discard",
          style: "destructive",
          // Read NOW, not when the alert opened: a flush can hand the photo to
          // the queue while this sits on screen.
          onPress: () => clearCapture(photoQueuedRef.current),
        },
      ],
    );
  }

  /**
   * Leaves the capture screen, letting go of the photo only when nobody else
   * needs it.
   *
   * A queued capture's photo now belongs to the queue and is shared by every
   * entry still waiting in it; anything else has either been uploaded or
   * abandoned, and its file is ours to clean up.
   */
  function clearCapture(photoBelongsToQueue: boolean): void {
    // Anything still in flight belongs to a capture that no longer exists.
    generation.current += 1;
    // Dealt with, one way or another: filed, queued, or discarded. Until this
    // point the pending list was still holding it, which is what made it
    // survive the app being killed mid-review.
    if (captureId !== "") releasePending(captureId);
    // And its finally can no longer clear this, precisely because of the line
    // above -- so clear it here. Without this, abandoning a capture during a
    // request leaves busy stuck true forever, and "Take photo" is disabled on
    // busy: the app looks bricked until it is reloaded.
    setBusy(false);
    if (!photoBelongsToQueue) deleteCapturePhoto(photo?.name);
    photoQueuedRef.current = false;
    setStep("capture");
    setCaptureId("");
    setPhoto(null);
    setDrafts([]);
    setExpandedId("");
    setRecs([]);
    setStale(false);
    setOfflinePicks(null);
    setChoices({});
    setOpenDestId("");
    setFiled([]);
    setFailures([]);
    setQueued([]);
    setPhotoQueued(false);
    setPhotoWarning("");
  }

  /**
   * "Catalog more" must not be the button that quietly throws away the items
   * Homebox refused. They exist nowhere else at this point.
   */
  /**
   * The containers just filed into whose fill is worth asking about -- nobody
   * knows it, or what is known is a sum of guesses getting full or stale.
   * Looked up in the freshest box data at hand: this screen's recommendations,
   * then the offline cache.
   */
  function boxesToCheck(): Box[] {
    const ids = new Set<string>();
    for (const entry of [...filed, ...queued]) if (entry.boxId) ids.add(entry.boxId);
    if (ids.size === 0) return [];
    const known = new Map<string, Box>();
    for (const b of cachedBoxes()) known.set(b.id, b);
    for (const rec of recs) for (const c of rec.candidates ?? []) known.set(c.box.id, c.box);
    const out: Box[] = [];
    for (const id of ids) {
      const b = known.get(id);
      if (b && wantsFillCheck(b)) out.push(b);
    }
    return out;
  }

  function leaveDone(): void {
    if (failures.length === 0) {
      clearCapture(photoQueued);
      return;
    }
    Alert.alert(
      `${failures.length} ${plural(failures.length, "item has", "items have")} not been filed`,
      `${plural(failures.length, "It is", "They are")} not in Homebox and ${plural(failures.length, "exists", "exist")} only on this phone. Keep ${plural(failures.length, "it", "them")} to retry later, or discard — discarding cannot be undone.`,
      [
        { text: "Cancel", style: "cancel" },
        {
          text: plural(failures.length, "Keep it", "Keep them"),
          onPress: () => {
            queueFailures();
            clearCapture(true);
          },
        },
        { text: "Discard", style: "destructive", onPress: () => clearCapture(photoQueued) },
      ],
    );
  }

  function confirmDiscard(entry: QueuedCapture) {
    const count = entry.entries?.length ?? 0;
    const first = entry.entries?.[0];
    const what =
      count === 1 && first ? `“${itemName(first.item)}”` : `${count} items from one photo`;
    Alert.alert(
      "Discard this capture?",
      `${what} ${plural(count, "has", "have")} not reached Homebox. Discarding cannot be undone.`,
      [
        { text: "Keep it", style: "cancel" },
        { text: "Discard", style: "destructive", onPress: () => discard(entry.captureId) },
      ],
    );
  }

  // -------------------------------------------------------------------------
  // Render
  // -------------------------------------------------------------------------

  // Why the capture on the waiting screen is still waiting, when it has tried
  // and failed in a way worth retrying.
  const watchedError =
    (watching !== "" && pending.captures.find((c) => c.captureId === watching)?.error) || "";

  const single = drafts.length === 1;
  const readyEntries = step === "recommend" ? entriesFor(choices) : null;

  /**
   * Where back goes from here, or null on a screen that is already the start.
   *
   * Two of these are free and one is not. Returning to the item list keeps
   * every draft and every destination already chosen -- nothing is thrown
   * away, the recommendations are simply asked for again on the way forward.
   * Leaving the item list is the one that discards a photo somebody has
   * already taken, so it asks first, and it is worded as what it does rather
   * than as a direction.
   */
  // Not offered while a request is in flight. The generation guard above makes
  // leaving mid-request SAFE, but leaving mid-request is still not something to
  // invite: the user would be walking away from a file that may be seconds from
  // landing, and the alternative is a spinner they cannot escape for 60s. So it
  // is shown greyed rather than removed, which keeps the header from jumping.
  const back: { label: string; onPress: () => void } | null =
    step === "recommend"
      ? { label: "Items", onPress: () => setStep("review") }
      : step === "inbox"
        ? { label: "Back", onPress: () => setStep("capture") }
        : step === "setup"
          ? { label: "Back", onPress: exitSetup }
          : step === "review"
            ? { label: "Start over", onPress: confirmAbandon }
            : null;

  return (
    <View style={styles.root}>
      {/* Light only until there is a dark palette: every colour below is
          hard-coded for a white background, and "auto" would put white
          status-bar text on it for anyone whose phone is in dark mode. */}
      <StatusBar barStyle="dark-content" />
      {/*
        Every screen past the first is reached by going forward, and until now
        none of them offered a way back: picking a destination, or opening the
        location picker, was a one-way door out of a capture you were part way
        through. The label says where it goes, rather than a bare chevron,
        because "back" from the destination screen and "back" from the location
        picker land somewhere different.
      */}
      <View style={styles.header}>
        {back !== null ? (
          <Pressable
            style={styles.back}
            accessibilityRole="button"
            accessibilityLabel={back.label}
            accessibilityState={{ disabled: busy }}
            disabled={busy}
            onPress={back.onPress}
          >
            <Text style={[styles.backText, busy && styles.disabled]}>‹ {back.label}</Text>
          </Pressable>
        ) : null}
        <Text style={styles.title}>Boxwright</Text>
      </View>

      {(queue.entries.length > 0 || queue.lastError !== "") && (
        <View style={styles.banner}>
          <Pressable style={styles.bannerRow} onPress={() => setShowQueue(!showQueue)}>
            <Text style={styles.bannerText}>
              {queue.entries.length === 0
                ? "Queue problem"
                : `${queue.entries.length} ${plural(queue.entries.length, "capture", "captures")} waiting to upload`}
              {queue.flushing ? " · sending…" : ""}
            </Text>
            {queue.entries.length > 0 && <Text style={styles.bannerLink}>{showQueue ? "Hide" : "Show"}</Text>}
          </Pressable>
          {queue.lastError !== "" && !queue.flushing && (
            <Text style={styles.bannerError}>Last attempt failed: {queue.lastError}</Text>
          )}
          {showQueue &&
            queue.entries.map((entry) => (
              <View key={entry.captureId} style={styles.queueRow}>
                <View style={styles.queueRowText}>
                  {(entry.entries ?? []).map((queuedEntry, i) => (
                    <Text key={`${entry.captureId}-${i}`} style={styles.queueName}>
                      {quantityPrefix(queuedEntry.item)}
                      {itemName(queuedEntry.item)} → {queuedEntry.boxName || "unrecorded box"}
                    </Text>
                  ))}
                  <Text style={styles.queueMeta}>
                    {ago(entry.queuedAt)}
                    {entry.attempts > 0 ? ` · ${entry.attempts} ${plural(entry.attempts, "attempt", "attempts")}` : ""}
                    {entry.photoName ? "" : " · no photo"}
                  </Text>
                  {entry.lastError !== "" && <Text style={styles.queueError}>{entry.lastError}</Text>}
                </View>
                <Pressable style={styles.queueDiscard} onPress={() => confirmDiscard(entry)}>
                  <Text style={styles.queueDiscardText}>Discard</Text>
                </Pressable>
              </View>
            ))}
          {queue.entries.length > 0 && (
            <Pressable disabled={queue.flushing} onPress={() => void flushQueue()}>
              <Text style={[styles.bannerLink, queue.flushing && styles.disabled]}>Retry now</Text>
            </Pressable>
          )}
        </View>
      )}

      {!server.loaded && <ActivityIndicator style={styles.spinner} size="large" />}

      {(needsServer || step === "connection") && (
        <ConnectionScreen firstRun={needsServer} onDone={connectionSaved} />
      )}

      {server.loaded && !needsServer && step === "capture" && (
        <View style={styles.center}>
          <Text style={styles.lede}>
            Photograph an item, a shelf, or a whole container — or use a picture you already have.
          </Text>
          {placeCount === 0 && (
            /* Without this the app looks broken in a specific and misleading
               way: every item comes back "no existing container fits",
               forever, and nothing says why. */
            <>
              <Text style={styles.hint}>
                Boxwright does not know where it may put things yet. Choose the locations in your
                Homebox that it is allowed to file into — a shelf, a room, a cupboard, a stack of
                bins, whatever you actually use.
              </Text>
              <Pressable style={styles.primary} onPress={() => setStep("setup")}>
                <Text style={styles.primaryText}>Choose locations</Text>
              </Pressable>
            </>
          )}

          {/*
            The camera stays the primary action and the other two are quieter,
            because photographing the thing in front of you is what this is
            for. But the thing is not always in front of you: the box may be
            taped shut and the photo taken last week, or the picture may be a
            product listing you saved or copied. All three land in the same
            place.
          */}
          <Pressable
            style={[placeCount === 0 ? styles.secondary : styles.primary, busy && styles.disabled]}
            onPress={() => void takePhoto()}
            disabled={busy}
            accessibilityRole="button"
          >
            <Text style={placeCount === 0 ? styles.secondaryText : styles.primaryText}>
              {placeCount === 0 ? "Take a photo anyway" : "Take photo"}
            </Text>
          </Pressable>

          <Pressable
            style={[styles.secondary, busy && styles.disabled]}
            onPress={() => void chooseExisting()}
            disabled={busy}
            accessibilityRole="button"
          >
            <Text style={styles.secondaryText}>Choose an existing photo</Text>
          </Pressable>

          <Pressable onPress={() => void pasteImage()} disabled={busy} accessibilityRole="button">
            <Text style={[styles.bannerLink, busy && styles.disabled]}>Paste a copied image</Text>
          </Pressable>

          {pending.captures.length > 0 && (
            <Pressable
              onPress={() => setStep("inbox")}
              disabled={busy}
              accessibilityRole="button"
              accessibilityState={{ disabled: busy }}
            >
              <Text style={[styles.bannerLink, busy && styles.disabled]}>
                {pendingSummary(pending.captures)}
              </Text>
            </Pressable>
          )}

          {placeCount > 0 && (
            <Pressable onPress={() => setStep("setup")} accessibilityRole="button">
              <Text style={styles.bannerLink}>
                Filing into {placeCount} {plural(placeCount, "location", "locations")} · change
              </Text>
            </Pressable>
          )}

          <Pressable onPress={() => setStep("connection")} accessibilityRole="button">
            <Text style={styles.bannerLink}>Server settings</Text>
          </Pressable>
        </View>
      )}

      {step === "waiting" && (
        <View style={styles.center}>
          {photo && <Image source={{ uri: photo.uri }} style={styles.photo} />}
          <ActivityIndicator />
          <Text style={styles.lede}>Working out what is in this photo…</Text>
          {watchedError !== "" ? (
            <>
              {/* Recorded on disk and, until now, shown nowhere: somebody in a
                  storage unit with no signal watched a spinner that would
                  never resolve and were told nothing. */}
              <Text style={styles.needsAttention}>{watchedError}</Text>
              <Pressable
                style={styles.secondary}
                accessibilityRole="button"
                onPress={() => void runIdentification()}
              >
                <Text style={styles.secondaryText}>Try again now</Text>
              </Pressable>
            </>
          ) : (
            <Text style={styles.hint}>
              About five seconds for each thing in the picture, so a single item is quick and a
              whole shelf is not. You do not have to wait for it.
            </Text>
          )}
          {/*
            The escape hatch, and the reason any of this exists. Parking keeps
            the photo, keeps the request, and gives the camera back -- so a room
            can be photographed in one pass and reviewed sitting down.
          */}
          <Pressable
            style={styles.primary}
            accessibilityRole="button"
            onPress={() => {
              setWatching("");
              setPhoto(null);
              setCaptureId("");
              setStep("capture");
            }}
          >
            <Text style={styles.primaryText}>Take another</Text>
          </Pressable>
        </View>
      )}

      {step === "inbox" && (
        <ScrollView contentContainerStyle={styles.form} automaticallyAdjustKeyboardInsets>
          <Text style={styles.lede}>Photos waiting</Text>
          {pending.captures.length === 0 && (
            <Text style={styles.hint}>Nothing waiting. Every photo you took has been reviewed.</Text>
          )}
          {pending.captures.map((capture) => {
            const uri = capture.photoName ? storedPhotoUri(capture.photoName) : null;
            const ready = capture.status === "ready";
            const failed = capture.status === "failed";
            const count = capture.items?.length ?? 0;
            return (
              <View key={capture.captureId} style={styles.itemBlock}>
                <View style={styles.itemHeader}>
                  {uri !== null && <Image source={{ uri }} style={styles.itemThumb} />}
                  <View style={styles.itemHeaderText}>
                    <Text style={styles.cardTitle}>
                      {ready
                        ? `${count} ${plural(count, "item", "items")}`
                        : failed
                          ? "Could not identify"
                          : pending.working === capture.captureId
                            ? "Identifying…"
                            : "Waiting"}
                    </Text>
                    <Text style={capture.error ? styles.needsAttention : styles.cardBody}>
                      {capture.error ? capture.error : ago(capture.queuedAt)}
                    </Text>
                  </View>
                </View>
                <View style={styles.inboxActions}>
                  {ready && (
                    <Pressable
                      style={styles.primary}
                      accessibilityRole="button"
                      onPress={() => openPending(capture)}
                    >
                      <Text style={styles.primaryText}>Review</Text>
                    </Pressable>
                  )}
                  {/* Offered for a stalled capture as well as a failed one.
                      A retriable failure leaves the status as "identifying"
                      with the reason recorded, and without this the only way
                      out was to background the app or take another photo --
                      neither of them discoverable from here. */}
                  {(failed || (!ready && capture.error !== "")) && (
                    <Pressable
                      style={styles.secondary}
                      accessibilityRole="button"
                      onPress={() => {
                        if (failed) retryPending(capture.captureId);
                        void runIdentification();
                      }}
                    >
                      <Text style={styles.secondaryText}>Try again</Text>
                    </Pressable>
                  )}
                  <Pressable
                    style={styles.removeButton}
                    accessibilityRole="button"
                    accessibilityLabel="Discard this photo"
                    onPress={() =>
                      Alert.alert("Discard this photo?", "The picture will be deleted.", [
                        { text: "Keep", style: "cancel" },
                        {
                          text: "Discard",
                          style: "destructive",
                          onPress: () => discardPending(capture.captureId),
                        },
                      ])
                    }
                  >
                    <Text style={styles.removeText}>Discard</Text>
                  </Pressable>
                </View>
              </View>
            );
          })}
        </ScrollView>
      )}

      {step === "setup" && (
        <LocationPicker onDone={exitSetup} />
      )}

      {step === "review" && (
        <ScrollView contentContainerStyle={styles.form} automaticallyAdjustKeyboardInsets keyboardShouldPersistTaps="handled">
          {photo && <Image source={{ uri: photo.uri }} style={styles.photo} />}
          {photo && !photo.downscaled && (
            <Text style={styles.hint}>Could not resize this photo; it will upload at full size.</Text>
          )}
          {drafts.length > 1 && (
            <Text style={styles.hint}>
              {drafts.length} things in this photo. Each is filed on its own, so they can end up in
              different boxes. Tap one to edit it, remove anything that is not really there, add
              anything missed.
            </Text>
          )}
          {drafts.map((draft, index) => (
            <ItemCard
              key={draft.id}
              draft={draft}
              index={index}
              // One item is the common case: no header, no chevron, no remove
              // button -- just the form, exactly as it has always been.
              alone={single}
              expanded={single || expandedId === draft.id}
              categories={categories}
              categoriesLive={categoriesLive}
              thumbnail={thumbnailFor(draft.item.region)}
              onToggle={() => setExpandedId(expandedId === draft.id ? "" : draft.id)}
              onRemove={() => confirmRemove(draft, index)}
              onChange={(patch) => updateDraft(draft.id, patch)}
              onCategory={(key, label) => chooseCategory(draft.id, key, label)}
            />
          ))}
          <Pressable style={styles.addItem} onPress={addItem} accessibilityRole="button">
            <Text style={styles.addItemText}>+ Add an item</Text>
          </Pressable>
          {/*
            Category is required, not defaulted. It is the engine's dominant
            signal and it becomes a tag in Homebox, so filling it in for the
            user would be both a worse recommendation and a tag they never
            chose. The button says which item is missing what.

            With a list, an incomplete item can be collapsed and off screen, so
            the button stays tappable and opens it. With one item there is
            nothing to open and nowhere to scroll, so it is simply disabled,
            exactly as it has always been.
          */}
          <Pressable
            style={[styles.primary, (drafts.length === 0 || firstIncomplete() !== null) && styles.disabled]}
            onPress={getRecommendation}
            disabled={busy || drafts.length === 0 || (single && firstIncomplete() !== null)}
          >
            <Text style={styles.primaryText}>{reviewButtonLabel()}</Text>
          </Pressable>
        </ScrollView>
      )}

      {step === "recommend" && (
        <ScrollView contentContainerStyle={styles.form} automaticallyAdjustKeyboardInsets>
          {stale && <Text style={styles.hint}>Showing cached boxes; Homebox is unreachable.</Text>}
          {offlinePicks !== null && (
            <Text style={styles.hint}>
              Offline. This is your saved box list, not a recommendation — nothing has been scored.
              Pick a box and the capture waits on this phone until Homebox can be reached.
            </Text>
          )}
          {/*
            Never promises a box has been picked when one has not: with no
            candidates at all the top suggestion does not exist, and saying it
            does would send someone to the bottom button to be refused there.
          */}
          {!single && drafts.length > 0 && (
            <Text style={styles.hint}>
              {readyEntries
                ? "Each item has a box picked for it already. Tap “Change” to send one somewhere else, or file them all as they are."
                : "Each item goes into its own box. Tap the ones still marked below and choose where they go."}
            </Text>
          )}
          {drafts.map((draft, index) => (
            <DestinationBlock
              key={draft.id}
              draft={draft}
              alone={single}
              busy={busy}
              chosen={choices[draft.id]}
              open={single || openDestId === draft.id}
              recommendation={recs[index]}
              picks={offlinePicks?.[index]}
              categories={categories}
              onToggle={() => setOpenDestId(openDestId === draft.id ? "" : draft.id)}
              onChoose={(destination) => choose(draft, destination)}
            />
          ))}
          {!single && drafts.length > 0 && (
            <Pressable
              style={[styles.primary, (busy || readyEntries === null) && styles.disabled]}
              disabled={busy || readyEntries === null}
              onPress={() => {
                if (readyEntries) void submit(readyEntries);
              }}
            >
              <Text style={styles.primaryText}>
                {readyEntries ? `File all ${drafts.length} items` : "Every item needs a box"}
              </Text>
            </Pressable>
          )}
        </ScrollView>
      )}

      {step === "done" && (
        <ScrollView contentContainerStyle={styles.form} automaticallyAdjustKeyboardInsets>
          <Text style={styles.lede}>{doneHeadline()}</Text>

          {filed.length > 1 &&
            filed.map((entry, i) => (
              <Text key={`filed-${i}`} style={styles.doneRow}>
                {quantityPrefix(entry.item)}
                {itemName(entry.item)} → {entry.boxName}
              </Text>
            ))}

          {failures.length > 0 && (
            <View style={styles.failureBlock}>
              <Text style={styles.failureHead}>
                Not filed — {plural(failures.length, "this item is", "these items are")} only on this
                phone:
              </Text>
              {failures.map((failure, i) => (
                <Text key={`failed-${i}`} style={styles.failureRow}>
                  {itemName(failure.entry.item)} → {failure.entry.boxName}: {failure.error}
                </Text>
              ))}
              <Pressable style={styles.primary} onPress={queueFailures}>
                <Text style={styles.primaryText}>
                  Keep {failures.length === 1 ? "it" : `all ${failures.length}`} on this phone
                </Text>
              </Pressable>
            </View>
          )}

          {queued.length > 0 && filed.length > 0 && (
            <Text style={styles.hint}>
              {queued.length} {plural(queued.length, "item is", "items are")} waiting on this phone
              and will file {plural(queued.length, "itself", "themselves")} when Homebox is
              reachable.
            </Text>
          )}

          <FillCheck boxes={boxesToCheck()} captureId={captureId} queued={queued} />

          {photoWarning !== "" && <Text style={styles.hint}>{photoWarning}</Text>}

          <Pressable
            style={failures.length > 0 ? styles.secondary : styles.primary}
            onPress={leaveDone}
          >
            <Text style={failures.length > 0 ? styles.secondaryText : styles.primaryText}>
              Catalog something else
            </Text>
          </Pressable>
        </ScrollView>
      )}

      <DepthCaptureModal
        visible={depthCamera}
        mode="item"
        onCapture={(shot) => void depthShot(shot)}
        onCancel={() => setDepthCamera(false)}
        onSystemCamera={() => {
          setDepthCamera(false);
          void systemCamera();
        }}
      />

      {busy && <ActivityIndicator style={styles.spinner} size="large" />}
    </View>
  );
}

/**
 * One item on the review screen: a summary row that opens into the full form.
 *
 * `alone` collapses the whole apparatus away. A photo of one thing is the
 * common case and it deserves the form it has always had -- no header to tap
 * past, no remove button beside it, nothing to serve the eight-item case at
 * its expense.
 */
/**
 * Choose which locations Boxwright may file into.
 *
 * Every location in the user's Homebox is listed, indented by depth, with
 * nothing filtered out. Boxwright used to work this out for itself -- a
 * location was a container if it had no locations under it, was not an
 * unannotated top-level entry, and so on -- and every one of those rules was
 * generalised from a single inventory. Against a flat Homebox of twenty empty
 * boxes they all said no, so the engine reported that nothing fitted and
 * suggested buying another container, forever.
 *
 * Whether somewhere is a place you put things is not a fact about its shape.
 * It is a decision, and it is the user's.
 */
function LocationPicker(props: { onDone: () => void }) {
  const [sizing, setSizing] = useState(false);
  const [rows, setRows] = useState<LocationNode[] | null>(null);
  const [error, setError] = useState("");
  /** Only what the user CHANGED. Each one is a write, so the untouched ones are not sent. */
  const [changed, setChanged] = useState<Record<string, boolean>>({});
  const [busy, setBusy] = useState(false);
  const [note, setNote] = useState("");

  const load = async () => {
    setError("");
    try {
      const res = await fetchLocations();
      setRows(res.locations);
      setChanged({});
    } catch (err) {
      setRows([]);
      setError(errorMessage(err));
    }
  };

  useEffect(() => {
    void load();
  }, []);

  if (sizing) return <ContainerSetup onDone={() => setSizing(false)} />;

  const isOn = (row: LocationNode) => changed[row.id] ?? row.eligible;
  const list = rows ?? [];
  const chosen = list.filter(isOn).length;
  const pending = Object.keys(changed).length;

  async function save() {
    const body = Object.entries(changed).map(([id, eligible]) => ({ id, eligible }));
    if (body.length === 0) {
      props.onDone();
      return;
    }
    setBusy(true);
    setNote("");
    try {
      const res = await setLocations(body);
      const failed = res.results.filter((r) => (r.error ?? "") !== "");
      if (failed.length > 0) {
        // Partial success is normal here and must not be reported as either a
        // clean save or a clean failure: the ones that landed have landed.
        setError(
          `${failed.length} of ${body.length} could not be saved: ${failed[0]?.error ?? "unknown error"}`,
        );
        await load();
        return;
      }
      props.onDone();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }

  async function adopt() {
    setBusy(true);
    setError("");
    setNote("");
    try {
      const res = await adoptLocations();
      setNote(
        res.marked === 0
          ? "Nothing to bring over — no locations had container settings on them already."
          : `Brought over ${res.marked} ${plural(res.marked, "location", "locations")} that already had container settings.`,
      );
      await load();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <ScrollView contentContainerStyle={styles.form} automaticallyAdjustKeyboardInsets keyboardShouldPersistTaps="handled">
      <Text style={styles.lede}>Where may Boxwright file things?</Text>
      <Text style={styles.hint}>
        Tick anywhere you would actually put something. Boxwright will only ever recommend these,
        and will leave the rest of your Homebox alone.
      </Text>

      {rows === null && <ActivityIndicator />}
      {error !== "" && (
        <View style={styles.failureBlock}>
          <Text style={styles.failureRow}>{error}</Text>
          <Pressable onPress={() => void load()}>
            <Text style={styles.bannerLink}>Try again</Text>
          </Pressable>
        </View>
      )}
      {note !== "" && <Text style={styles.hintNew}>{note}</Text>}

      {rows !== null && list.length === 0 && error === "" && (
        <Text style={styles.hint}>
          There are no locations in your Homebox yet. Create one there first — a room, a shelf, a
          box, whatever fits how you already organise — and come back.
        </Text>
      )}

      {list.map((row) => {
        const on = isOn(row);
        return (
          <Pressable
            key={row.id}
            style={[styles.locationRow, on && styles.locationRowOn, { paddingLeft: 12 + row.depth * 16 }]}
            onPress={() =>
              setChanged((prev) => {
                const next = { ...prev };
                // Ticking something back to where it started is not a change,
                // and sending it would be a pointless write to Homebox.
                if (!on === row.eligible) delete next[row.id];
                else next[row.id] = !on;
                return next;
              })
            }
          >
            <Text style={styles.locationCheck}>{on ? "☑" : "☐"}</Text>
            <View style={styles.locationText}>
              <Text style={styles.locationName}>{row.name}</Text>
              <Text style={styles.locationMeta}>
                {row.parentName ? `in ${row.parentName}` : "top level"}
                {row.hasChildren ? " · holds other locations" : ""}
                {row.itemCount > 0
                  ? ` · ${row.itemCount} ${plural(row.itemCount, "item", "items")}`
                  : ""}
              </Text>
            </View>
          </Pressable>
        );
      })}

      {list.length > 0 && (
        <Text style={styles.hint}>
          {chosen} of {list.length} selected
          {pending > 0 ? ` · ${pending} unsaved ${plural(pending, "change", "changes")}` : ""}
        </Text>
      )}

      <Pressable style={[styles.primary, busy && styles.disabled]} onPress={() => void save()} disabled={busy}>
        <Text style={styles.primaryText}>{pending > 0 ? "Save selection" : "Done"}</Text>
      </Pressable>

      {/*
        Sizes are recorded on the containers already chosen, so this comes
        after the selection -- and unsaved ticks must be saved first, or the
        container someone just ticked would be missing from the list.
      */}
      <Pressable
        style={[styles.secondary, (busy || pending > 0) && styles.disabled]}
        onPress={() => setSizing(true)}
        disabled={busy || pending > 0}
      >
        <Text style={styles.secondaryText}>
          {pending > 0 ? "Save your selection to record sizes" : "Record container sizes"}
        </Text>
      </Pressable>

      {/* For anyone upgrading, and for anyone who filled the capacity and
          access fields in by hand: it never overrides a choice already made. */}
      <Pressable style={[styles.secondary, busy && styles.disabled]} onPress={() => void adopt()} disabled={busy}>
        <Text style={styles.secondaryText}>Bring over locations I already set up</Text>
      </Pressable>

      <Pressable onPress={props.onDone}>
        <Text style={styles.bannerLink}>Cancel</Text>
      </Pressable>
    </ScrollView>
  );
}

/**
 * Record what kind of container each chosen location is: its type, how much
 * it holds, its inside size -- and, optionally, how easy it is to reach and
 * how full it is right now.
 *
 * Several at once, because a storage unit is usually many of the same thing:
 * tick every 27-gallon one, say so once. The type names are the user's own; the
 * ones offered are the ones already on their containers, and nothing is
 * shipped. Everything is optional except a capacity for a new type, and only
 * what is filled in is written -- nothing else about a container is touched.
 */
function ContainerSetup(props: { onDone: () => void }) {
  const [boxes, setBoxes] = useState<Box[] | null>(null);
  const [types, setTypes] = useState<ContainerType[]>([]);
  const [error, setError] = useState("");
  const [note, setNote] = useState("");
  const [busy, setBusy] = useState(false);
  const [selected, setSelected] = useState<Record<string, boolean>>({});
  /** "" = nothing chosen, "\u0000new" = a new type, otherwise a type name. */
  const [typeChoice, setTypeChoice] = useState("");
  const [newName, setNewName] = useState("");
  const [capacityText, setCapacityText] = useState("");
  const [interiorText, setInteriorText] = useState("");
  const [access, setAccess] = useState<Access | undefined>(undefined);
  const [fill, setFill] = useState<number | undefined>(undefined);

  const load = async () => {
    setError("");
    try {
      const res = await fetchBoxList();
      setBoxes(res.boxes.filter((b) => !b.isArea));
      setTypes(Array.isArray(res.containerTypes) ? res.containerTypes : []);
    } catch (err) {
      setBoxes([]);
      setError(errorMessage(err));
    }
  };
  useEffect(() => {
    void load();
  }, []);

  const list = boxes ?? [];
  const ids = list.filter((b) => selected[b.id]).map((b) => b.id);
  const isNew = typeChoice === NEW_TYPE;
  const existing = types.find((t) => t.name === typeChoice);

  /** What will be written, or why not. */
  function plan(): { set: ContainerSet } | { problem: string } {
    const set: ContainerSet = {};
    if (isNew) {
      const name = newName.trim();
      if (name === "") return { problem: "Name the new type, as you would call it." };
      const litres = parseCapacity(capacityText);
      if (litres === undefined) return { problem: "Say how much it holds: litres, or gallons like \"27 gal\"." };
      set.containerType = name;
      set.capacityL = litres;
      if (interiorText.trim() !== "") {
        const inside = parseDims(interiorText);
        if (!inside) return { problem: "Inside size is three numbers in centimetres, like 70 x 45 x 38." };
        set.interiorCm = inside;
      }
    } else if (existing) {
      set.containerType = existing.name;
      set.capacityL = existing.capacityL;
      if (existing.interiorCm) set.interiorCm = existing.interiorCm;
    }
    if (access !== undefined) set.access = access;
    if (fill !== undefined) set.fill = { pct: fill, source: "observed", at: new Date().toISOString() };
    if (Object.keys(set).length === 0) return { problem: "Choose a type, or what to record." };
    return { set };
  }

  async function apply() {
    const p = plan();
    if ("problem" in p) {
      setError(p.problem);
      return;
    }
    setBusy(true);
    setError("");
    setNote("");
    try {
      const res = await setContainers({ ids, set: p.set });
      const failed = res.results.filter((r) => (r.error ?? "") !== "");
      const saved = res.results.length - failed.length;
      if (failed.length > 0) {
        setError(`${failed.length} of ${ids.length} could not be saved: ${failed[0]?.error ?? "unknown error"}`);
      }
      if (saved > 0) setNote(`Recorded on ${saved} ${plural(saved, "container", "containers")}.`);
      setSelected({});
      setFill(undefined);
      await load();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <ScrollView contentContainerStyle={styles.form} automaticallyAdjustKeyboardInsets keyboardShouldPersistTaps="handled">
      <Text style={styles.lede}>What kind of container is each one?</Text>
      <Text style={styles.hint}>
        Tick the ones that are the same, then say what they are. Boxwright uses the size to tell
        when something will not fit, and never guesses one you have not given.
      </Text>

      {boxes === null && <ActivityIndicator />}
      {error !== "" && (
        <View style={styles.failureBlock}>
          <Text style={styles.failureRow}>{error}</Text>
        </View>
      )}
      {note !== "" && <Text style={styles.hintNew}>{note}</Text>}
      {boxes !== null && list.length === 0 && error === "" && (
        <Text style={styles.hint}>Choose where Boxwright may file things first; their sizes are recorded here.</Text>
      )}

      {list.map((b) => {
        const on = selected[b.id] === true;
        const known = [
          b.containerType || "",
          typeof b.capacityL === "number" ? `${b.capacityL} L` : "",
          b.interiorCm ? formatDims(b.interiorCm) : "",
          fillSummary(b) ?? "",
        ].filter((part) => part !== "");
        return (
          <Pressable
            key={b.id}
            style={[styles.locationRow, on && styles.locationRowOn]}
            accessibilityRole="checkbox"
            accessibilityState={{ checked: on }}
            onPress={() => setSelected((prev) => ({ ...prev, [b.id]: !on }))}
          >
            <Text style={styles.locationCheck}>{on ? "☑" : "☐"}</Text>
            <View style={styles.locationText}>
              <Text style={styles.locationName}>{b.name}</Text>
              <Text style={styles.locationMeta}>
                {b.area ? `in ${b.area} · ` : ""}
                {known.length > 0 ? known.join(" · ") : "size not recorded"}
              </Text>
            </View>
          </Pressable>
        );
      })}

      {ids.length > 0 && (
        <View style={styles.field}>
          <Text style={styles.fieldLabel}>
            {ids.length} {plural(ids.length, "container is", "containers are")} a…
          </Text>
          <View style={styles.chips}>
            {[...types.map((t) => t.name), NEW_TYPE].map((name) => {
              const chosen = typeChoice === name;
              const label = name === NEW_TYPE ? "New type…" : name;
              return (
                <Pressable
                  key={name}
                  style={[styles.chip, chosen && styles.chipSelected]}
                  accessibilityRole="button"
                  accessibilityState={{ selected: chosen }}
                  onPress={() => setTypeChoice(chosen ? "" : name)}
                >
                  <Text style={[styles.chipText, chosen && styles.chipTextSelected]}>{label}</Text>
                </Pressable>
              );
            })}
          </View>
          {existing && (
            <Text style={styles.hint}>
              {existing.capacityL} L{existing.interiorCm ? `, ${formatDims(existing.interiorCm)} inside` : ""}
            </Text>
          )}
          {isNew && (
            <>
              <Field label="Type name, as you call it" value={newName} onChange={setNewName} />
              <Field label="How much it holds (litres, or gallons like 27 gal)" value={capacityText} onChange={setCapacityText} />
              <Field label="Inside size in cm (optional), like 70 x 45 x 38" value={interiorText} onChange={setInteriorText} />
            </>
          )}

          <Text style={styles.fieldLabel}>How easy to reach (optional)</Text>
          <View style={styles.chips}>
            {(["easy", "normal", "deep"] as const).map((a) => {
              const chosen = access === a;
              return (
                <Pressable
                  key={a}
                  style={[styles.chip, chosen && styles.chipSelected]}
                  accessibilityRole="button"
                  accessibilityState={{ selected: chosen }}
                  onPress={() => setAccess(chosen ? undefined : a)}
                >
                  <Text style={[styles.chipText, chosen && styles.chipTextSelected]}>{a}</Text>
                </Pressable>
              );
            })}
          </View>

          <Text style={styles.fieldLabel}>How full right now (optional)</Text>
          <View style={styles.chips}>
            {FILL_CHOICES.map(([label, pct]) => {
              const chosen = fill === pct;
              return (
                <Pressable
                  key={label}
                  style={[styles.chip, chosen && styles.chipSelected]}
                  accessibilityRole="button"
                  accessibilityState={{ selected: chosen }}
                  onPress={() => setFill(chosen ? undefined : pct)}
                >
                  <Text style={[styles.chipText, chosen && styles.chipTextSelected]}>{label}</Text>
                </Pressable>
              );
            })}
          </View>

          <Pressable style={[styles.primary, busy && styles.disabled]} onPress={() => void apply()} disabled={busy}>
            <Text style={styles.primaryText}>
              Record on {ids.length} {plural(ids.length, "container", "containers")}
            </Text>
          </Pressable>
        </View>
      )}

      <Pressable onPress={props.onDone}>
        <Text style={styles.bannerLink}>Back to choosing locations</Text>
      </Pressable>
    </ScrollView>
  );
}

const NEW_TYPE = "\u0000new";

function ItemCard(props: {
  draft: Draft;
  index: number;
  alone: boolean;
  expanded: boolean;
  categories: CategoryOption[];
  categoriesLive: boolean;
  /**
   * The photo cropped to THIS item, when the model said where it is.
   * Undefined is the common case; the whole photo is shown above the list
   * either way, so there is nothing to substitute here.
   */
  thumbnail?: Thumbnail;
  onToggle: () => void;
  onRemove: () => void;
  onChange: (patch: Partial<ItemDraft>) => void;
  onCategory: (key: string, label: string) => void;
}) {
  const draft = props.draft;
  const item = draft.item;
  const incomplete = item.name.trim() === "" || item.category === "";
  const summary = [
    item.category === "" ? "" : categoryLabel(item.category, props.categories),
    // The bucket measures an item against a container, so it says nothing
    // about something that does not go in one.
    item.bulky ? "too big for a box" : sizePhrase(item),
    item.weightClass,
    item.fragile ? "fragile" : "",
  ]
    .filter((part) => part !== "")
    .join(" · ");

  return (
    <View style={props.alone ? styles.plainBlock : styles.itemBlock}>
      {!props.alone && (
        <View style={styles.itemHeader}>
          {/*
            The picture of THIS thing, next to the row that names it. Eight
            items from one shelf shot otherwise all carry the same photo of the
            shelf, which says nothing about which row is which -- and the row
            that most needs a picture is the one the model was least sure of.
            Decorative: the text beside it already says everything, so it is
            hidden from a screen reader rather than given a label that repeats.
          */}
          {props.thumbnail !== undefined && (
            <View
              style={[styles.itemThumb, styles.itemThumbClip]}
              accessibilityElementsHidden
              importantForAccessibility="no-hide-descendants"
            >
              <Image
                source={{ uri: props.thumbnail.uri }}
                style={[
                  styles.itemThumbImage,
                  coverCrop(props.thumbnail.crop, props.thumbnail.photo, ITEM_THUMB_SIDE),
                ]}
              />
            </View>
          )}
          <Pressable
            style={styles.itemHeaderText}
            accessibilityRole="button"
            accessibilityState={{ expanded: props.expanded }}
            onPress={props.onToggle}
          >
            <Text style={styles.cardTitle}>
              {quantityPrefix(item)}
              {item.name.trim() || `Item ${props.index + 1}`}
            </Text>
            <Text style={incomplete ? styles.needsAttention : styles.cardBody}>
              {item.name.trim() === ""
                ? "Needs a name"
                : item.category === ""
                  ? "Needs a category"
                  : summary}
            </Text>
          </Pressable>
          {/*
            Kept apart from the row that expands the item, with its own
            padding: the two live side by side and only one of them is
            recoverable from.
          */}
          <Pressable
            style={styles.removeButton}
            accessibilityRole="button"
            accessibilityLabel={`Remove ${item.name.trim() || `item ${props.index + 1}`}`}
            onPress={props.onRemove}
          >
            <Text style={styles.removeText}>Remove</Text>
          </Pressable>
        </View>
      )}

      {props.expanded && (
        <View style={styles.itemBody}>
          {item.confidence > 0 && (
            <Text style={styles.hint}>
              Identified with {(item.confidence * 100).toFixed(0)}% confidence. Edit anything below.
            </Text>
          )}
          <Field label="Name" value={item.name} onChange={(name) => props.onChange({ name })} />
          <CategoryPicker
            options={props.categories}
            live={props.categoriesLive}
            value={item.category}
            proposed={draft.proposed && draft.proposed.key === item.category ? draft.proposed : null}
            onChange={props.onCategory}
          />
          <Field label="Notes" value={item.notes} onChange={(notes) => props.onChange({ notes })} />
          <Toggle
            label={`Size: ${item.sizeBucket}`}
            onPress={() =>
              props.onChange({
                sizeBucket:
                  item.sizeBucket === "S" ? "M" : item.sizeBucket === "M" ? "L" : item.sizeBucket === "L" ? "XL" : "S",
              })
            }
          />
          {!item.bulky && (
            <SizeField
              dims={item.dimensionsCm ?? null}
              source={item.dimensionsSource}
              onChange={(dims) =>
                props.onChange(
                  dims ? { dimensionsCm: dims, dimensionsSource: "manual" } : { dimensionsCm: null, dimensionsSource: undefined },
                )
              }
            />
          )}
          <Toggle
            label={`Weight: ${item.weightClass}`}
            onPress={() =>
              props.onChange({
                weightClass:
                  item.weightClass === "light" ? "medium" : item.weightClass === "medium" ? "heavy" : "light",
              })
            }
          />
          <Toggle
            label={item.fragile ? "Fragile: yes" : "Fragile: no"}
            onPress={() => props.onChange({ fragile: !item.fragile })}
          />
          {/*
            Correctable, because it decides between two completely different
            answers: a container, or somewhere to stand it. The size buckets
            above stop meaning anything once this is on.
          */}
          <Toggle
            label={item.bulky ? "Too big for a box: yes" : "Too big for a box: no"}
            onPress={() => props.onChange({ bulky: !item.bulky })}
          />
          <View style={styles.stepper}>
            <Text style={styles.fieldLabel}>How many</Text>
            <Pressable
              style={styles.stepperButton}
              accessibilityRole="button"
              accessibilityLabel="One fewer"
              onPress={() => props.onChange({ quantity: Math.max(1, item.quantity - 1) })}
            >
              <Text style={styles.toggleText}>−</Text>
            </Pressable>
            <Text style={styles.stepperValue}>{item.quantity}</Text>
            <Pressable
              style={styles.stepperButton}
              accessibilityRole="button"
              accessibilityLabel="One more"
              onPress={() => props.onChange({ quantity: item.quantity + 1 })}
            >
              <Text style={styles.toggleText}>+</Text>
            </Pressable>
          </View>
        </View>
      )}
    </View>
  );
}

/**
 * Where one item is going, and the alternatives.
 *
 * With one item this is the old screen: a list of boxes, and tapping one files
 * it. With several, tapping a box chooses it and a single button at the bottom
 * files everything -- because tapping through eight identical confirmations is
 * how a tool stops being used.
 */
function DestinationBlock(props: {
  draft: Draft;
  alone: boolean;
  busy: boolean;
  chosen: Destination | undefined;
  open: boolean;
  recommendation: Recommendation | undefined;
  picks: Box[] | undefined;
  categories: CategoryOption[];
  onToggle: () => void;
  onChoose: (destination: Destination) => void;
}) {
  const item = props.draft.item;
  const candidates = props.recommendation?.candidates ?? [];
  const offline = props.picks ?? [];
  const suggestion = props.recommendation?.newContainer;
  // Set only for something that does not go in a container and has nowhere to
  // stand -- a lawn mower with no garage. Offering to create a box for it
  // would be the same mistake that made this necessary.
  const noPlace = props.recommendation?.noPlace;
  const empty = candidates.length === 0 && offline.length === 0 && !suggestion && !noPlace;

  return (
    <View style={props.alone ? styles.plainBlock : styles.itemBlock}>
      {!props.alone && (
        <Pressable style={styles.itemHeader} accessibilityRole="button" onPress={props.onToggle}>
          <View style={styles.itemHeaderText}>
            <Text style={styles.cardTitle}>
              {quantityPrefix(item)}
              {item.name}
            </Text>
            <Text style={props.chosen ? styles.cardBody : styles.needsAttention}>
              {props.chosen ? `→ ${props.chosen.name}` : "No box for this one yet"}
            </Text>
          </View>
          <Text style={styles.bannerLink}>{props.open ? "Done" : "Change"}</Text>
        </Pressable>
      )}

      {props.open && (
        <View style={styles.itemBody}>
          {candidates.map((candidate) => {
            const destination: Destination = {
              kind: "box",
              boxId: candidate.box.id,
              name: candidate.box.name,
            };
            return (
              <DestinationCard
                key={candidate.box.id}
                title={`${candidate.box.name}${candidate.box.area ? ` · ${candidate.box.area}` : ""}`}
                body={(candidate.reasons ?? []).join(" · ")}
                selected={!props.alone && sameDestination(props.chosen, destination)}
                busy={props.busy}
                onPress={() => props.onChoose(destination)}
              />
            );
          })}

          {offline.map((box) => {
            const destination: Destination = { kind: "box", boxId: box.id, name: box.name };
            return (
              <DestinationCard
                key={box.id}
                title={`${box.name}${box.area ? ` · ${box.area}` : ""}`}
                body={
                  (box.categories?.[normalizeCategory(item.category)] ?? 0) > 0
                    ? `Already holds ${categoryLabel(item.category, props.categories).toLowerCase()}`
                    : "No matching contents recorded"
                }
                selected={!props.alone && sameDestination(props.chosen, destination)}
                busy={props.busy}
                onPress={() => props.onChoose(destination)}
              />
            );
          })}

          {suggestion && (
            <DestinationCard
              title={`Start a new ${suggestion.sizeBucket} container`}
              body={`${suggestion.reason}. It will be labelled “${suggestion.label}” and belongs somewhere ${accessPhrase(suggestion.access)}.`}
              selected={!props.alone && props.chosen?.kind === "new"}
              busy={props.busy}
              onPress={() => props.onChoose(newContainerDestination(suggestion, props.recommendation))}
            />
          )}

          {noPlace && (
            <View style={styles.failureBlock}>
              <Text style={styles.failureHead}>Too big for a container</Text>
              <Text style={styles.failureRow}>{noPlace.reason}</Text>
            </View>
          )}
          {empty && <Text style={styles.hint}>No boxes found. Add locations in Homebox first.</Text>}
        </View>
      )}
    </View>
  );
}

function DestinationCard(props: {
  title: string;
  body: string;
  selected: boolean;
  busy: boolean;
  onPress: () => void;
}) {
  return (
    <Pressable
      style={[styles.card, props.selected && styles.cardSelected, props.busy && styles.disabled]}
      accessibilityRole="button"
      accessibilityState={{ selected: props.selected }}
      disabled={props.busy}
      onPress={props.onPress}
    >
      <Text style={styles.cardTitle}>
        {props.selected ? "✓ " : ""}
        {props.title}
      </Text>
      {props.body !== "" && <Text style={styles.cardBody}>{props.body}</Text>}
    </Pressable>
  );
}

/**
 * "How full is it now?" for each container something was just put into.
 *
 * Asked here, after filing, rather than before: a photo of one item files the
 * moment its destination is tapped, and the answer is most accurate once the
 * item is actually in. It is optional -- nothing here blocks anything -- and
 * an answer is an OBSERVATION the server keeps over its own estimates. While
 * the capture is still waiting on this phone the answer rides with it;
 * otherwise it is saved straight away.
 */
function FillCheck(props: { boxes: Box[]; captureId: string; queued: QueuedEntry[] }) {
  const [status, setStatus] = useState<Record<string, string>>({});
  /** Containers whose fill is dealt with: saved, or riding with the queue. */
  const [settled, setSettled] = useState<Record<string, boolean>>({});
  /** The container being measured with the depth camera, if any. */
  const [measuring, setMeasuring] = useState<Box | null>(null);
  if (props.boxes.length === 0) return null;

  /**
   * A depth photo of the open container from above, read against its inside
   * size. The photo and its depth are thrown away as soon as the number is
   * out: only the percentage goes anywhere.
   */
  async function measured(box: Box, shot: DepthCapture): Promise<void> {
    setMeasuring(null);
    const inside = normalizeDims(box.interiorCm);
    if (!shot.depthPath || !inside) {
      discardTempFiles(shot.photoPath, shot.depthPath);
      setStatus((s) => ({ ...s, [box.id]: "No depth in that photo. Choose how full it is instead." }));
      return;
    }
    const res = await measureFillFromFile(shot.depthPath, inside);
    discardTempFiles(shot.photoPath, shot.depthPath);
    if ("refused" in res) {
      setStatus((s) => ({ ...s, [box.id]: `${res.refused} Or choose how full it is.` }));
      return;
    }
    await record(box, res.fillPct, "lidar");
  }

  async function record(box: Box, pct: number, source: FillObservation["source"] = "observed"): Promise<void> {
    const fill: FillObservation = { pct, source, at: new Date().toISOString() };
    const waiting = props.queued.some((e) => e.boxId === box.id);
    if (waiting && setQueuedFill(props.captureId, box.id, fill)) {
      setStatus((s) => ({ ...s, [box.id]: "Noted. It goes with the items when they file." }));
      setSettled((s) => ({ ...s, [box.id]: true }));
      return;
    }
    setStatus((s) => ({ ...s, [box.id]: source === "lidar" ? `Measured about ${Math.round(pct)}% full. Saving…` : "Saving…" }));
    try {
      const res = await setContainers({ ids: [box.id], set: { fill } });
      const error = res.results[0]?.error;
      if (error) throw new Error(error);
      setStatus((s) => ({ ...s, [box.id]: source === "lidar" ? `Measured about ${Math.round(pct)}% full. Saved.` : "Saved." }));
      setSettled((s) => ({ ...s, [box.id]: true }));
    } catch {
      setStatus((s) => ({ ...s, [box.id]: "Not saved; it will be asked again next time." }));
    }
  }

  return (
    <View style={styles.field}>
      {props.boxes.map((box) => (
        <View key={box.id} style={styles.field}>
          <Text style={styles.fieldLabel}>
            How full is {box.name} now?{fillSummary(box) ? ` Boxwright thinks ${fillSummary(box)}.` : ""}
          </Text>
          {settled[box.id] ? (
            <Text style={styles.hint}>{status[box.id]}</Text>
          ) : (
            <View style={styles.chips}>
              {status[box.id] && <Text style={styles.hint}>{status[box.id]}</Text>}
              {/*
                Measuring needs the inside size -- depth to the contents means
                nothing without depth to the bottom -- and a phone with LiDAR.
              */}
              {DepthKit.isSupported && normalizeDims(box.interiorCm) && (
                <Pressable
                  style={styles.chip}
                  accessibilityRole="button"
                  accessibilityLabel={`Measure how full ${box.name} is with the camera`}
                  onPress={() => setMeasuring(box)}
                >
                  <Text style={styles.chipText}>Measure</Text>
                </Pressable>
              )}
              {FILL_CHOICES.map(([label, pct]) => (
                <Pressable
                  key={label}
                  style={styles.chip}
                  accessibilityRole="button"
                  accessibilityLabel={`${box.name} is ${label === "Empty" || label === "Full" ? label.toLowerCase() : `${pct}% full`}`}
                  onPress={() => void record(box, pct)}
                >
                  <Text style={styles.chipText}>{label}</Text>
                </Pressable>
              ))}
            </View>
          )}
        </View>
      ))}
      <DepthCaptureModal
        visible={measuring !== null}
        mode="fill"
        onCapture={(shot) => {
          if (measuring) void measured(measuring, shot);
        }}
        onCancel={() => setMeasuring(null)}
      />
    </View>
  );
}

/**
 * Boxwright's own camera: an ARKit preview that keeps the LiDAR depth with the
 * photo, full screen over whatever opened it.
 *
 * Item mode is "Take photo" on a LiDAR phone, with a way back to the system
 * camera for anyone who prefers it. Fill mode is for measuring how full an
 * open container is: it draws the guide the measurement reads (FILL_GUIDE)
 * and shows how far from straight down the phone is, because past 30 degrees
 * the measurement refuses. The session runs only while this is showing.
 */
function DepthCaptureModal(props: {
  visible: boolean;
  mode: DepthCameraMode;
  onCapture: (shot: DepthCapture) => void;
  onCancel: () => void;
  onSystemCamera?: () => void;
}) {
  const camera = useRef<DepthCameraRef | null>(null);
  const [status, setStatus] = useState<DepthStatus | null>(null);
  const [torch, setTorch] = useState(false);
  const [taking, setTaking] = useState(false);
  const fill = props.mode === "fill";

  async function shoot(): Promise<void> {
    if (!camera.current || taking) return;
    setTaking(true);
    try {
      props.onCapture(await camera.current.capture());
    } catch (err) {
      Alert.alert("Could not take the photo", errorMessage(err));
    } finally {
      setTaking(false);
    }
  }

  const tilt = status ? Math.round(status.tiltFromDownDeg) : undefined;
  const hint = !status
    ? "Starting the camera…"
    : status.tracking !== "normal"
      ? "Move the phone slowly so it can find its bearings."
      : fill
        ? tilt !== undefined && tilt > 30
          ? `Tilted ${tilt}° — hold it more flat, looking straight down.`
          : "Frame the open container inside the box, looking straight down."
        : status.depthOK
          ? "Ready."
          : "Too close or too far for depth; the photo still works.";

  return (
    <Modal visible={props.visible} animationType="slide" presentationStyle="fullScreen" onRequestClose={props.onCancel}>
      <View style={styles.cameraRoot}>
        {props.visible && (
          <DepthCamera
            style={StyleSheet.absoluteFill}
            mode={props.mode}
            active={props.visible}
            torch={torch}
            onStatus={callback((s: DepthStatus) => setStatus(s))}
            hybridRef={callback((ref: DepthCameraRef) => {
              camera.current = ref;
            })}
          />
        )}
        {fill && (
          <View
            pointerEvents="none"
            style={[
              styles.cameraGuide,
              {
                left: `${FILL_GUIDE.x * 100}%`,
                top: `${FILL_GUIDE.y * 100}%`,
                width: `${FILL_GUIDE.w * 100}%`,
                height: `${FILL_GUIDE.h * 100}%`,
              },
            ]}
          />
        )}
        <Text style={styles.cameraHint}>{hint}</Text>
        <View style={styles.cameraBar}>
          <Pressable accessibilityRole="button" onPress={props.onCancel} style={styles.cameraSide}>
            <Text style={styles.cameraText}>Cancel</Text>
          </Pressable>
          <Pressable
            accessibilityRole="button"
            accessibilityLabel={fill ? "Measure" : "Take photo"}
            onPress={() => void shoot()}
            disabled={taking}
            style={[styles.shutter, taking && styles.disabled]}
          />
          <Pressable
            accessibilityRole="button"
            accessibilityState={{ selected: torch }}
            onPress={() => setTorch((t) => !t)}
            style={styles.cameraSide}
          >
            <Text style={styles.cameraText}>{torch ? "Light on" : "Light"}</Text>
          </Pressable>
        </View>
        {props.onSystemCamera && (
          <Pressable accessibilityRole="button" onPress={props.onSystemCamera} style={styles.cameraSystem}>
            <Text style={styles.cameraText}>Use the system camera</Text>
          </Pressable>
        )}
      </View>
    </Modal>
  );
}

const FILL_CHOICES: [string, number][] = [
  ["Empty", 0],
  ["¼", 25],
  ["½", 50],
  ["¾", 75],
  ["Full", 100],
];

/** An item's size for a summary line: measured, estimated, or its bucket. */
function sizePhrase(item: ItemDraft): string {
  const dims = normalizeDims(item.dimensionsCm);
  if (!dims) return item.sizeBucket;
  return item.dimensionsSource === "vision" ? `≈${formatDims(dims)}` : formatDims(dims);
}

/**
 * The item's size in centimetres, as text a person can correct.
 *
 * Kept as text while it is being typed and read only when they finish: "30 x"
 * is on its way to being a size, not a wrong one. What they type is a
 * MEASUREMENT (source "manual") -- the engine will then rule out a container
 * it does not fit -- so the label says where the current figure came from,
 * and an estimate is marked as one. Clearing the field goes back to the size
 * bucket.
 */
function SizeField(props: {
  dims: Dims | null;
  source: ItemDraft["dimensionsSource"];
  onChange: (dims: Dims | null) => void;
}) {
  const shown = props.dims ? `${props.dims.l} x ${props.dims.w} x ${props.dims.h}` : "";
  const [text, setText] = useState(shown);
  const [bad, setBad] = useState(false);
  // A new estimate or a LiDAR measurement replaces what was shown.
  useEffect(() => {
    setText(shown);
    setBad(false);
  }, [shown]);

  const origin =
    props.source === "lidar"
      ? "measured with LiDAR"
      : props.source === "manual"
        ? "as you entered it"
        : props.source === "vision"
          ? "estimated from the photo; correct it if you know"
          : "optional; the size above is used without it";

  function commit(): void {
    if (text.trim() === "") {
      setBad(false);
      if (props.dims) props.onChange(null);
      return;
    }
    const parsed = parseDims(text);
    if (!parsed) {
      setBad(true);
      return;
    }
    setBad(false);
    props.onChange(parsed);
  }

  return (
    <View style={styles.field}>
      <Text style={styles.fieldLabel}>Size in cm — {origin}</Text>
      <TextInput
        style={styles.input}
        value={text}
        onChangeText={setText}
        onEndEditing={commit}
        onSubmitEditing={commit}
        placeholder="30 x 20 x 10"
        keyboardType="numbers-and-punctuation"
        returnKeyType="done"
        accessibilityLabel="Size in centimetres, three numbers"
      />
      {bad && <Text style={styles.needsAttention}>Three numbers in centimetres, like 30 x 20 x 10.</Text>}
    </View>
  );
}

function Field(props: { label: string; value: string; onChange: (v: string) => void }) {
  return (
    <View style={styles.field}>
      <Text style={styles.fieldLabel}>{props.label}</Text>
      <TextInput style={styles.input} value={props.value} onChangeText={props.onChange} />
    </View>
  );
}

/**
 * Category as chips over the user's OWN vocabulary, plus a way in for one more.
 *
 * Chips, not a modal or a wheel: this is used standing in a storage unit with a
 * box under one arm, so every option is one thumb-sized tap away with nothing to
 * open or dismiss. The list is served most-used-first, which puts the handful of
 * categories somebody actually files into within reach of a thumb.
 *
 * Open, not closed. The vocabulary is the user's Homebox tags and the model is
 * allowed to propose a category none of them cover, so a picker that could only
 * choose would silently drop that proposal on the floor. "New category" is
 * therefore always available -- last in the row, and never the default, because
 * inventing a near-duplicate of an existing tag is the one way this control can
 * do damage.
 */
function CategoryPicker(props: {
  options: CategoryOption[];
  live: boolean;
  value: string;
  proposed: Proposed | null;
  onChange: (key: string, label: string) => void;
}) {
  const [adding, setAdding] = useState(false);
  const [typed, setTyped] = useState("");

  const shown = pickerOptions(props.options, props.value, props.proposed?.label ?? "");
  // Judged against the live list, not against `proposed`, so a category that
  // arrives in a refresh quietly stops being announced as new.
  // "New" means "filing this will create a Homebox tag", which is true both
  // for a category nobody has offered AND for one that exists in the list but
  // has nothing filed under it yet -- the server reports the latter as
  // inUse:false. Treating only the first as new made an unused seed look like
  // an established category, so a user picking it had no idea a tag was about
  // to appear in their Homebox.
  const isNew = (key: string) => {
    const option = props.options.find((o) => o.key === key);
    return !option || option.inUse === false;
  };

  function commit() {
    const label = typed.trim();
    if (label === "") {
      setAdding(false);
      return;
    }
    // Typing a category that already exists selects it instead of proposing a
    // second one: "Tools" and "tools" are one tag in Homebox, and two chips for
    // it would split the affinity the engine reads off that tag. Matching has
    // to consider LABELS as well as keys -- the chips show "Books & Media"
    // while the wire carries "books-media", so typing exactly what is on
    // screen would otherwise invent a new category.
    const existing = resolveTypedCategory(label, props.options);
    props.onChange(existing?.key ?? categoryKeyFor(label), existing?.label ?? label);
    setTyped("");
    setAdding(false);
  }

  return (
    <View style={styles.field}>
      <Text style={styles.fieldLabel}>Category</Text>
      {!props.live && (
        <Text style={styles.hint}>
          Showing the starter list — your Homebox categories could not be loaded.
        </Text>
      )}
      {props.proposed !== null && isNew(props.value) && (
        <Text style={styles.hintNew}>
          {props.proposed.byModel
            ? `Suggested “${props.proposed.label}”, which is not one of your categories yet. Filing this will create it — keep it, or tap another.`
            : `“${props.proposed.label}” will be created as a new category when you file this.`}
        </Text>
      )}
      <View style={styles.chips}>
        {shown.map((option) => {
          const selected = option.key === props.value;
          const fresh = isNew(option.key);
          return (
            <Pressable
              key={option.key}
              accessibilityRole="button"
              accessibilityState={{ selected }}
              accessibilityLabel={fresh ? `${option.label}, new category` : option.label}
              style={[styles.chip, fresh && styles.chipNew, selected && styles.chipSelected]}
              onPress={() => props.onChange(option.key, option.label)}
            >
              <Text style={[styles.chipText, selected && styles.chipTextSelected]}>
                {option.label}
                {fresh ? " · new" : ""}
              </Text>
            </Pressable>
          );
        })}
        {!adding && (
          <Pressable
            accessibilityRole="button"
            accessibilityLabel="Add a category that is not listed"
            style={[styles.chip, styles.chipAdd]}
            onPress={() => setAdding(true)}
          >
            <Text style={styles.chipAddText}>+ New category</Text>
          </Pressable>
        )}
      </View>
      {adding && (
        <View style={styles.addRow}>
          <TextInput
            style={[styles.input, styles.addInput]}
            value={typed}
            onChangeText={setTyped}
            placeholder="e.g. Appliances"
            autoFocus
            autoCapitalize="words"
            autoCorrect={false}
            returnKeyType="done"
            onSubmitEditing={commit}
          />
          <Pressable
            accessibilityRole="button"
            style={[styles.addButton, typed.trim() === "" && styles.disabled]}
            disabled={typed.trim() === ""}
            onPress={commit}
          >
            <Text style={styles.primaryText}>Use</Text>
          </Pressable>
          <Pressable
            accessibilityRole="button"
            style={styles.addCancel}
            onPress={() => {
              setTyped("");
              setAdding(false);
            }}
          >
            <Text style={styles.toggleText}>Cancel</Text>
          </Pressable>
        </View>
      )}
    </View>
  );
}

function Toggle(props: { label: string; onPress: () => void }) {
  return (
    <Pressable style={styles.toggle} onPress={props.onPress}>
      <Text style={styles.toggleText}>{props.label}</Text>
    </Pressable>
  );
}

const styles = StyleSheet.create({
  cameraRoot: { flex: 1, backgroundColor: "#000" },
  cameraGuide: { position: "absolute", borderWidth: 2, borderColor: "#fff", borderRadius: 6 },
  cameraHint: {
    position: "absolute",
    top: 64,
    left: 16,
    right: 16,
    color: "#fff",
    fontSize: 16,
    textAlign: "center",
    textShadowColor: "#000",
    textShadowRadius: 4,
  },
  cameraBar: {
    position: "absolute",
    bottom: 48,
    left: 0,
    right: 0,
    flexDirection: "row",
    alignItems: "center",
    justifyContent: "space-around",
  },
  cameraSide: { minWidth: 88, minHeight: 44, alignItems: "center", justifyContent: "center" },
  cameraText: { color: "#fff", fontSize: 17, textShadowColor: "#000", textShadowRadius: 4 },
  cameraSystem: { position: "absolute", bottom: 12, alignSelf: "center", minHeight: 32 },
  shutter: { width: 72, height: 72, borderRadius: 36, backgroundColor: "#fff", borderWidth: 4, borderColor: "#bbb" },
  root: { flex: 1, paddingTop: 64, paddingHorizontal: 20, backgroundColor: "#fff" },
  stepper: { flexDirection: "row", alignItems: "center", gap: 12, marginBottom: 12 },
  stepperButton: {
    width: 44, height: 44, borderRadius: 8, borderWidth: 1, borderColor: "#ccc",
    alignItems: "center", justifyContent: "center",
  },
  stepperValue: { fontSize: 17, minWidth: 28, textAlign: "center" },
  header: { flexDirection: "row", alignItems: "center", gap: 4, marginBottom: 12 },
  title: { fontSize: 22, fontWeight: "600" },
  // Sized for a thumb, not for the glyph: this sits next to nothing else, and
  // a mis-tap on it is a mis-tap out of a screen somebody was working in.
  back: { minHeight: 44, minWidth: 44, justifyContent: "center" },
  backText: { fontSize: 17, color: "#1a1a1a" },
  center: { flex: 1, justifyContent: "center", gap: 16 },
  lede: { fontSize: 17, lineHeight: 24 },
  form: { gap: 12, paddingBottom: 48 },
  photo: { width: "100%", height: 220, borderRadius: 8, backgroundColor: "#eee" },
  hint: { color: "#666", fontSize: 14 },
  field: { gap: 4 },
  fieldLabel: { fontSize: 14, color: "#333" },
  input: { borderWidth: 1, borderColor: "#ccc", borderRadius: 8, padding: 10, fontSize: 16 },
  // A single item gets no frame at all: it is the whole screen, not one row of
  // a list, and boxing it would be decoration charged to the common case.
  plainBlock: { gap: 12 },
  itemBlock: { borderWidth: 1, borderColor: "#e2e2e2", borderRadius: 10, padding: 12, gap: 10 },
  itemHeader: { flexDirection: "row", alignItems: "center", gap: 8 },
  itemThumb: { width: ITEM_THUMB_SIDE, height: ITEM_THUMB_SIDE, borderRadius: 6, backgroundColor: "#eee" },
  itemThumbClip: { overflow: "hidden" },
  itemThumbImage: { position: "absolute" },
  inboxActions: { gap: 8 },
  itemHeaderText: { flex: 1, gap: 2, minHeight: 44, justifyContent: "center" },
  itemBody: { gap: 12 },
  needsAttention: { fontSize: 14, color: "#8a4b0f" },
  removeButton: { minHeight: 44, paddingHorizontal: 10, justifyContent: "center" },
  removeText: { fontSize: 14, color: "#7a2f2f", textDecorationLine: "underline" },
  addItem: {
    borderWidth: 1, borderColor: "#999", borderStyle: "dashed", borderRadius: 8,
    minHeight: 48, alignItems: "center", justifyContent: "center",
  },
  addItemText: { fontSize: 16, color: "#555" },
  chips: { flexDirection: "row", flexWrap: "wrap", gap: 8 },
  chip: {
    borderWidth: 1, borderColor: "#ccc", borderRadius: 22, paddingHorizontal: 14,
    minHeight: 44, justifyContent: "center",
  },
  chipSelected: { backgroundColor: "#1a1a1a", borderColor: "#1a1a1a" },
  chipText: { fontSize: 16, color: "#1a1a1a" },
  chipTextSelected: { color: "#fff", fontWeight: "600" },
  // A category that does not exist yet reads differently from one that does,
  // whether or not the hint above has been read.
  chipNew: { borderStyle: "dashed", borderColor: "#8a6d1f", backgroundColor: "#fdf6e3" },
  chipAdd: { borderStyle: "dashed", borderColor: "#999" },
  chipAddText: { fontSize: 16, color: "#555" },
  addRow: { flexDirection: "row", alignItems: "center", gap: 8, marginTop: 4 },
  addInput: { flex: 1 },
  addButton: {
    backgroundColor: "#1a1a1a", borderRadius: 8, paddingHorizontal: 16,
    minHeight: 44, justifyContent: "center",
  },
  addCancel: { minHeight: 44, paddingHorizontal: 8, justifyContent: "center" },
  hintNew: { color: "#5c4a12", fontSize: 14 },
  toggle: { borderWidth: 1, borderColor: "#ccc", borderRadius: 8, padding: 10 },
  toggleText: { fontSize: 16 },
  primary: { backgroundColor: "#1a1a1a", borderRadius: 8, padding: 14, alignItems: "center" },
  primaryText: { color: "#fff", fontSize: 16, fontWeight: "600" },
  secondary: {
    borderWidth: 1, borderColor: "#1a1a1a", borderRadius: 8, padding: 14, alignItems: "center",
  },
  secondaryText: { color: "#1a1a1a", fontSize: 16, fontWeight: "600" },
  disabled: { opacity: 0.4 },
  card: { borderWidth: 1, borderColor: "#ddd", borderRadius: 8, padding: 14, gap: 4 },
  cardSelected: { borderColor: "#1a1a1a", borderWidth: 2, backgroundColor: "#f5f5f5" },
  cardTitle: { fontSize: 16, fontWeight: "600" },
  cardBody: { fontSize: 14, color: "#444" },
  doneRow: { fontSize: 15, color: "#333" },
  failureBlock: {
    borderWidth: 1, borderColor: "#e0a3a3", backgroundColor: "#fdf0f0",
    borderRadius: 8, padding: 12, gap: 8,
  },
  failureHead: { fontSize: 15, fontWeight: "600", color: "#7a2f2f" },
  failureRow: { fontSize: 14, color: "#7a2f2f" },
  banner: {
    borderWidth: 1, borderColor: "#e0c37a", backgroundColor: "#fdf6e3",
    borderRadius: 8, padding: 10, marginBottom: 12, gap: 6,
  },
  bannerRow: { flexDirection: "row", justifyContent: "space-between", alignItems: "center" },
  bannerText: { fontSize: 15, fontWeight: "600", color: "#5c4a12", flexShrink: 1 },
  bannerLink: { fontSize: 15, color: "#5c4a12", textDecorationLine: "underline" },
  bannerError: { fontSize: 13, color: "#7a2f2f" },
  queueRow: {
    flexDirection: "row", alignItems: "center", gap: 8,
    borderTopWidth: 1, borderTopColor: "#e8dcbb", paddingTop: 6,
  },
  queueRowText: { flex: 1, gap: 2 },
  queueName: { fontSize: 14, color: "#3a3115" },
  queueMeta: { fontSize: 12, color: "#7d6f45" },
  queueError: { fontSize: 12, color: "#7a2f2f" },
  queueDiscard: { minHeight: 44, justifyContent: "center", paddingHorizontal: 8 },
  queueDiscardText: { fontSize: 13, color: "#7a2f2f", textDecorationLine: "underline" },
  locationRow: {
    flexDirection: "row", alignItems: "center", gap: 10, minHeight: 52,
    paddingRight: 12, borderWidth: 1, borderColor: "#e2e2e2", borderRadius: 8,
  },
  locationRowOn: { borderColor: "#1a1a1a", backgroundColor: "#f5f5f5" },
  locationCheck: { fontSize: 20 },
  locationText: { flex: 1, gap: 2 },
  locationName: { fontSize: 16 },
  locationMeta: { fontSize: 12, color: "#666" },
  spinner: { position: "absolute", top: "50%", alignSelf: "center" },
});
