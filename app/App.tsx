import { useEffect, useRef, useState } from "react";
import { ActivityIndicator, Alert, AppState, Image, Linking, StatusBar, StyleSheet, View } from "react-native";
import { SafeAreaProvider } from "react-native-safe-area-context";
import Clipboard from "@react-native-clipboard/clipboard";
import { launchCamera, launchImageLibrary } from "react-native-image-picker";
import type { ImagePickerResponse } from "react-native-image-picker";
import { DepthKit } from "boxwright-depth";
import type { DepthCapture } from "boxwright-depth";

import { catalog, entryError, errorMessage, identify, isRetriable, landed, recommend } from "./src/api";
import { isConfigured, loadConnection, useConnection } from "./src/connection";
import { cropRect } from "./src/crop";
import { FALLBACK_CATEGORIES, isKnownCategory } from "./src/categories";
import {
  blankDraft,
  draftFromIdentified,
  entriesFor as entriesForCapture,
  firstIncomplete,
  itemName,
  photoMissWarning,
  plural,
  seedChoices,
} from "./src/draft";
import type { Destination, Draft, Failure } from "./src/draft";
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
import { wantsFillCheck } from "./src/capacity";
import { DepthCaptureModal } from "./src/components/DepthCaptureModal";
import type { Thumbnail } from "./src/components/ItemCard";
import { ConnectionScreen } from "./src/screens/ConnectionScreen";
import { DoneScreen, InboxScreen, RecommendScreen, ReviewScreen, WaitingScreen } from "./src/screens/CaptureScreens";
import { HomeScreen } from "./src/screens/HomeScreen";
import { LocationsScreen } from "./src/screens/LocationsScreen";
import { SetupFlow } from "./src/screens/SetupScreens";
import { prefs, setPrefs } from "./src/prefs";
import { startingStep } from "./src/setup";
import type { SetupStep } from "./src/setup";
import { forgetHomeboxTheme, refreshHomeboxTheme, useAccent } from "./src/theme/sync";
import { ThemeProvider } from "./src/theme/ThemeProvider";
import { palette } from "./src/theme/tokens";
import type {
  Box,
  CategoryOption,
  ItemDraft,
  PendingCapture,
  QueuedCapture,
  QueuedEntry,
  Recommendation,
} from "./src/types";

type Step = "capture" | "connection" | "setup" | "waiting" | "inbox" | "review" | "recommend" | "done";

export default function App() {
  const accent = useAccent();
  return (
    <SafeAreaProvider>
      <ThemeProvider accent={accent}>
        <Main />
      </ThemeProvider>
    </SafeAreaProvider>
  );
}

function Main() {
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
  /**
   * True once the offline queue holds this capture's photo. From that moment
   * the file belongs to the queue and this screen must never delete it: the
   * photo is shared by every entry still waiting, and taking it away would
   * strip the picture from all of them.
   */
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
  /**
   * First-run setup: the step showing, "done" once it is over, null until the
   * saved connection has been read -- deciding before then would put a
   * returning user through setup for the moment the Keychain takes.
   */
  const [setup, setSetup] = useState<SetupStep | "done" | null>(null);
  useEffect(() => {
    if (!server.loaded || setup !== null) return;
    const start = startingStep(prefs(), isConfigured(server.connection));
    // Somebody already using the app is recorded as set up, so that losing
    // the connection later does not send them back through the welcome.
    if (start === "done" && prefs().setupDone !== true) setPrefs({ setupDone: true });
    setSetup(start);
  }, [server.loaded, server.connection, setup]);

  function goSetup(next: SetupStep): void {
    setPrefs({ setupStep: next });
    setSetup(next);
  }

  function finishSetup(photo: boolean): void {
    setPrefs({ setupDone: true, setupStep: undefined });
    setSetup("done");
    exitSetup();
    if (photo) void takePhoto();
  }

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
      void refreshHomeboxTheme();
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
      // Another inventory, perhaps another person's Homebox: its colours are
      // not these.
      forgetHomeboxTheme();
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
    void refreshHomeboxTheme();
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

  async function getRecommendation() {
    const missing = firstIncomplete(drafts);
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

  /** Every item with its destination, or null when one is still undecided. See draft.entriesFor. */
  function entriesFor(map: Record<string, Destination>): QueuedEntry[] | null {
    return entriesForCapture(captureId, drafts, map);
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

  /**
   * Throws away a capture the user is part way through reviewing.
   *
   * Behind a confirmation because the photo goes with it and cannot be got
   * back -- the item may already be in the box by now. Nothing here has
   * reached Homebox or the queue, so this is a straight discard rather than
   * anything that needs unpicking.
   */
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

  /**
   * "Catalog more" must not be the button that quietly throws away the items
   * Homebox refused. They exist nowhere else at this point.
   */
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

  const readyEntries = step === "recommend" ? entriesFor(choices) : null;

  // Back is not offered while a request is in flight. The generation guard
  // makes leaving mid-request SAFE, but it is still not something to invite:
  // the user would be walking away from a file that may be seconds from
  // landing. So it is shown greyed rather than removed, which keeps the
  // header from jumping.
  //
  // Returning to the item list keeps every draft and every destination
  // already chosen. Leaving the item list is the one that discards a photo
  // somebody has already taken, so it asks first, and it is worded as what it
  // does rather than as a direction.

  function screen() {
    if (!server.loaded || setup === null) return <ActivityIndicator style={styles.spinner} size="large" />;
    if (setup !== "done") {
      return <SetupFlow step={setup} onStep={goSetup} onConnected={connectionSaved} onFinish={finishSetup} />;
    }
    if (needsServer || step === "connection") {
      return <ConnectionScreen firstRun={needsServer} onDone={connectionSaved} />;
    }
    switch (step) {
      case "capture":
        return (
          <HomeScreen
            busy={busy}
            placeCount={placeCount}
            pending={pending}
            queue={queue}
            onTakePhoto={() => void takePhoto()}
            onChooseExisting={() => void chooseExisting()}
            onPaste={() => void pasteImage()}
            onChooseLocations={() => setStep("setup")}
            onSettings={() => setStep("connection")}
            onOpenPending={openPending}
            onOpenInbox={() => setStep("inbox")}
            onRetryQueue={() => void flushQueue()}
            onDiscardQueued={confirmDiscard}
          />
        );
      case "waiting":
        return (
          <WaitingScreen
            photo={photo}
            error={watchedError}
            onRetry={() => void runIdentification()}
            onTakeAnother={() => {
              // The escape hatch, and the reason any of this exists. Parking
              // keeps the photo, keeps the request, and gives the camera back
              // -- so a room can be photographed in one pass and reviewed
              // sitting down.
              setWatching("");
              setPhoto(null);
              setCaptureId("");
              setStep("capture");
            }}
          />
        );
      case "inbox":
        return (
          <InboxScreen
            back={{ label: "Back", onPress: () => setStep("capture"), disabled: busy }}
            pending={pending}
            onOpen={openPending}
            onRetry={(capture) => {
              if (capture.status === "failed") retryPending(capture.captureId);
              void runIdentification();
            }}
            onDiscard={(capture) =>
              Alert.alert("Discard this photo?", "The picture will be deleted.", [
                { text: "Keep", style: "cancel" },
                { text: "Discard", style: "destructive", onPress: () => discardPending(capture.captureId) },
              ])
            }
          />
        );
      case "setup":
        return <LocationsScreen onDone={exitSetup} />;
      case "review":
        return (
          <ReviewScreen
            back={{ label: "Start over", onPress: confirmAbandon, disabled: busy }}
            busy={busy}
            photo={photo}
            drafts={drafts}
            expandedId={expandedId}
            categories={categories}
            categoriesLive={categoriesLive}
            thumbnailFor={thumbnailFor}
            onToggle={(id) => setExpandedId(expandedId === id ? "" : id)}
            onRemove={confirmRemove}
            onChange={updateDraft}
            onCategory={chooseCategory}
            onAdd={addItem}
            onNext={() => void getRecommendation()}
          />
        );
      case "recommend":
        return (
          <RecommendScreen
            back={{ label: "Items", onPress: () => setStep("review"), disabled: busy }}
            busy={busy}
            drafts={drafts}
            recs={recs}
            stale={stale}
            offlinePicks={offlinePicks}
            choices={choices}
            openDestId={openDestId}
            categories={categories}
            readyEntries={readyEntries}
            onToggle={(id) => setOpenDestId(openDestId === id ? "" : id)}
            onChoose={choose}
            onSubmit={(entries) => void submit(entries)}
          />
        );
      case "done":
        return (
          <DoneScreen
            captureId={captureId}
            filed={filed}
            failures={failures}
            queued={queued}
            photoWarning={photoWarning}
            checkBoxes={boxesToCheck()}
            onKeepFailures={queueFailures}
            onLeave={leaveDone}
          />
        );
      default:
        return null;
    }
  }

  return (
    <View style={styles.root}>
      <StatusBar barStyle="dark-content" />
      {screen()}
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
      {busy && step !== "review" && step !== "recommend" && (
        <ActivityIndicator style={styles.spinner} size="large" />
      )}
    </View>
  );
}

const styles = StyleSheet.create({
  root: { flex: 1, backgroundColor: palette.ground },
  spinner: { position: "absolute", top: "50%", alignSelf: "center" },
});
