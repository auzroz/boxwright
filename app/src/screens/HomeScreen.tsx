import { useState } from "react";
import { Image, Pressable, StyleSheet, Text, View } from "react-native";
import { DepthKit } from "boxwright-depth";

import { ago, itemName, plural, quantityPrefix } from "../draft";
import { storedPhotoUri } from "../photo";
import { useTheme } from "../theme/ThemeProvider";
import { MIN_TARGET, palette, radius, space, type } from "../theme/tokens";
import type { PendingCapture, PendingState, QueueState, QueuedCapture } from "../types";
import { Button } from "../ui/controls";
import { Icon } from "../ui/Icon";
import { Group, ListRow, Notice } from "../ui/surfaces";
import { Screen } from "../ui/Screen";

/** Past this many, the home screen shows the first few and a way to the rest. */
const SHOWN_PENDING = 3;

/**
 * Where every capture starts: one big button for the camera, quieter ways in
 * for a photo that already exists, and whatever is still waiting for the user.
 */
export function HomeScreen(props: {
  /** The demo: sample data, and copy that says so. */
  demo?: boolean;
  busy: boolean;
  placeCount: number;
  pending: PendingState;
  queue: QueueState;
  onTakePhoto: () => void;
  onChooseExisting: () => void;
  onPaste: () => void;
  onChooseLocations: () => void;
  onSettings: () => void;
  onOpenPending: (capture: PendingCapture) => void;
  onOpenInbox: () => void;
  onRetryQueue: () => void;
  onDiscardQueued: (entry: QueuedCapture) => void;
}) {
  const { accent } = useTheme();
  const noPlaces = props.placeCount === 0;
  const captures = props.pending.captures;

  return (
    <Screen>
      <View style={styles.brandRow}>
        <View style={[styles.brandMark, { backgroundColor: accent }]}>
          <Icon name="box" size={22} color={palette.onAccent} />
        </View>
        <Text style={[type.brand, styles.grow]} accessibilityRole="header">
          Boxwright
        </Text>
        <Pressable
          style={styles.settings}
          accessibilityRole="button"
          accessibilityLabel="Settings"
          onPress={props.onSettings}
        >
          <Icon name="settings" size={20} />
        </Pressable>
      </View>

      {noPlaces ? (
        /* Without this the app looks broken in a specific and misleading way:
           every item comes back "no existing container fits", forever, and
           nothing says why. */
        <Notice
          tone="caution"
          title="Choose where things may go"
          action={<Button label="Choose locations" compact onPress={props.onChooseLocations} />}
        >
          Boxwright does not know where it may put things yet. Pick the places in your Homebox it may
          file into: a shelf, a room, a cupboard, a stack of bins, whatever you actually use.
        </Notice>
      ) : (
        <View style={styles.filingRow}>
          <Text style={type.callout}>
            Filing into {props.placeCount} {plural(props.placeCount, "location", "locations")}
          </Text>
          <Pressable style={styles.inlineAction} accessibilityRole="button" accessibilityLabel="Change locations" onPress={props.onChooseLocations}>
            <Text style={[styles.inlineActionText, { color: accent }]}>Change</Text>
          </Pressable>
        </View>
      )}

      {/*
        The camera stays the primary action and the other two are quieter,
        because photographing the thing in front of you is what this is for.
        But the thing is not always in front of you: the box may be taped shut
        and the photo taken last week, or the picture may be a product listing
        you saved or copied. All three land in the same place.
      */}
      <Pressable
        accessibilityRole="button"
        accessibilityLabel={noPlaces ? "Take a photo anyway" : "Take photo"}
        accessibilityState={{ disabled: props.busy }}
        disabled={props.busy}
        onPress={props.onTakePhoto}
        style={({ pressed }) => [
          styles.capture,
          { backgroundColor: accent },
          pressed && styles.pressed,
          props.busy && styles.disabled,
        ]}
      >
        <View style={styles.captureIcon}>
          <Icon name="camera" size={28} color={palette.onAccent} />
        </View>
        <View style={styles.captureText}>
          <Text style={styles.captureTitle}>{noPlaces ? "Take a photo anyway" : "Take photo"}</Text>
          <Text style={styles.captureBody}>
            {props.demo
              ? "Try it on anything: the demo always finds a drill, some jars and string lights."
              : `One item, a shelf, or a whole container. ${DepthKit.isSupported ? "Each thing is measured with LiDAR." : "Sizes are estimated from the photo."}`}
          </Text>
        </View>
      </Pressable>

      <Group inset={52}>
        <ListRow icon="photo" title="Choose an existing photo" chevron disabled={props.busy} onPress={props.onChooseExisting} />
        <ListRow icon="paste" title="Paste a copied image" chevron disabled={props.busy} onPress={props.onPaste} />
      </Group>

      {(captures.length > 0 || props.queue.entries.length > 0 || props.queue.lastError !== "") && (
        <View style={styles.section}>
          <Text style={type.section}>WAITING FOR YOU</Text>
          <Group inset={72}>
            {captures.slice(0, SHOWN_PENDING).map((capture) => (
              <PendingRow
                key={capture.captureId}
                capture={capture}
                working={props.pending.working === capture.captureId}
                disabled={props.busy}
                onPress={() => (capture.status === "ready" ? props.onOpenPending(capture) : props.onOpenInbox())}
              />
            ))}
            {captures.length > SHOWN_PENDING && (
              <ListRow
                title={`See all ${captures.length} photos`}
                chevron
                disabled={props.busy}
                onPress={props.onOpenInbox}
              />
            )}
            {(props.queue.entries.length > 0 || props.queue.lastError !== "") && (
              <QueueRow queue={props.queue} onRetry={props.onRetryQueue} onDiscard={props.onDiscardQueued} />
            )}
          </Group>
        </View>
      )}
    </Screen>
  );
}

function PendingRow(props: { capture: PendingCapture; working: boolean; disabled: boolean; onPress: () => void }) {
  const c = props.capture;
  const count = c.items?.length ?? 0;
  const title =
    c.status === "ready"
      ? count === 1 && c.items?.[0]
        ? `${quantityPrefix(c.items[0])}${itemName(c.items[0])}`
        : `${count} ${plural(count, "item", "items")} found`
      : c.status === "failed"
        ? "Could not identify"
        : props.working
          ? "Identifying…"
          : "Waiting to identify";
  const detail = c.error ? c.error : c.status === "ready" ? `Ready to review · ${ago(c.queuedAt)}` : ago(c.queuedAt);
  return (
    <ListRow
      title={title}
      detail={detail}
      detailTone={c.error ? "warn" : "muted"}
      leading={<Image source={{ uri: storedPhotoUri(c.photoName) }} style={styles.thumb} />}
      chevron
      disabled={props.disabled}
      onPress={props.onPress}
    />
  );
}

/**
 * Captures that were filed on this phone and are waiting for the server. The
 * row opens into the list, where each can be discarded; Retry sends now
 * instead of waiting for the next launch.
 */
function QueueRow(props: { queue: QueueState; onRetry: () => void; onDiscard: (entry: QueuedCapture) => void }) {
  const { accent } = useTheme();
  const [open, setOpen] = useState(false);
  const n = props.queue.entries.length;
  return (
    <View>
      <View style={styles.queueHead}>
        <Pressable
          style={styles.queueText}
          accessibilityRole="button"
          accessibilityState={{ expanded: open }}
          onPress={() => setOpen(!open)}
          disabled={n === 0}
        >
          <Text style={type.title}>
            {n === 0 ? "Upload problem" : `${n} ${plural(n, "capture", "captures")} to upload`}
          </Text>
          <Text style={[type.label, props.queue.lastError !== "" && !props.queue.flushing && styles.warnText]}>
            {props.queue.flushing
              ? "Sending…"
              : props.queue.lastError !== ""
                ? `Last try failed: ${props.queue.lastError}`
                : "Saved on this phone. Files itself when the server is reachable."}
          </Text>
        </Pressable>
        {n > 0 && (
          <Pressable
            accessibilityRole="button"
            accessibilityLabel="Retry uploading now"
            disabled={props.queue.flushing}
            onPress={props.onRetry}
            style={[styles.retry, { borderColor: accent }, props.queue.flushing && styles.disabled]}
          >
            <Text style={[styles.retryText, { color: accent }]}>Retry</Text>
          </Pressable>
        )}
      </View>
      {open &&
        props.queue.entries.map((entry) => (
          <View key={entry.captureId} style={styles.queued}>
            <View style={styles.grow}>
              {(entry.entries ?? []).map((e, i) => (
                <Text key={`${entry.captureId}-${i}`} style={type.body}>
                  {quantityPrefix(e.item)}
                  {itemName(e.item)} → {e.boxName || "unrecorded place"}
                </Text>
              ))}
              <Text style={type.caption}>
                {ago(entry.queuedAt)}
                {entry.attempts > 0 ? ` · ${entry.attempts} ${plural(entry.attempts, "try", "tries")}` : ""}
                {entry.photoName ? "" : " · no photo"}
              </Text>
              {entry.lastError !== "" && <Text style={[type.caption, styles.warnText]}>{entry.lastError}</Text>}
            </View>
            <Pressable
              style={styles.inlineAction}
              accessibilityRole="button"
              accessibilityLabel="Discard this capture"
              onPress={() => props.onDiscard(entry)}
            >
              <Text style={[styles.inlineActionText, styles.warnText]}>Discard</Text>
            </Pressable>
          </View>
        ))}
    </View>
  );
}

const styles = StyleSheet.create({
  grow: { flex: 1 },
  brandRow: { flexDirection: "row", alignItems: "center", gap: space.md, marginTop: space.sm },
  brandMark: { width: 36, height: 36, borderRadius: 10, alignItems: "center", justifyContent: "center" },
  settings: {
    width: MIN_TARGET,
    height: MIN_TARGET,
    borderRadius: MIN_TARGET / 2,
    borderWidth: 1,
    borderColor: palette.line,
    backgroundColor: palette.surface,
    alignItems: "center",
    justifyContent: "center",
  },
  filingRow: { flexDirection: "row", alignItems: "center", justifyContent: "space-between", marginTop: -space.sm },
  inlineAction: { minHeight: MIN_TARGET, justifyContent: "center", paddingHorizontal: space.xs },
  inlineActionText: { fontSize: 15, fontWeight: "600" },
  capture: { borderRadius: radius.xxl, padding: space.xxl, gap: 14, minHeight: 184 },
  captureIcon: {
    width: 52,
    height: 52,
    borderRadius: 26,
    backgroundColor: "rgba(255,255,255,0.18)",
    alignItems: "center",
    justifyContent: "center",
  },
  captureText: { gap: space.xs },
  captureTitle: { ...type.heading, fontSize: 26, lineHeight: 31, color: palette.onAccent },
  captureBody: { fontSize: 15, lineHeight: 21, color: palette.onAccent, opacity: 0.92 },
  pressed: { opacity: 0.88 },
  disabled: { opacity: 0.4 },
  section: { gap: 10 },
  thumb: { width: 44, height: 44, borderRadius: 10, backgroundColor: palette.placeholder },
  queueHead: { flexDirection: "row", alignItems: "center", gap: space.md, paddingHorizontal: space.lg, paddingVertical: space.md },
  queueText: { flex: 1, gap: 2 },
  retry: { minHeight: MIN_TARGET, paddingHorizontal: 14, borderRadius: 22, borderWidth: 1, justifyContent: "center" },
  retryText: { fontSize: 15, fontWeight: "600" },
  warnText: { color: palette.warn },
  queued: {
    flexDirection: "row",
    alignItems: "center",
    gap: space.sm,
    paddingHorizontal: space.lg,
    paddingVertical: space.sm,
    borderTopWidth: StyleSheet.hairlineWidth,
    borderTopColor: palette.line,
  },
});
