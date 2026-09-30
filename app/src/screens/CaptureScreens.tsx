// The screens a capture passes through after the photo: waiting for the
// model, the list of parked photos, reviewing what was found, choosing where
// each thing goes, and the result. Each takes its state and its actions as
// props; App.tsx owns the capture and decides which one is showing.

import { ActivityIndicator, Image, StyleSheet, Text, View } from "react-native";

import { plural, itemName, quantityPrefix, reviewButtonLabel, firstIncomplete, ago } from "../draft";
import type { Destination, Draft, Failure } from "../draft";
import { storedPhotoUri } from "../photo";
import type { PersistedPhoto } from "../photo";
import { useTheme } from "../theme/ThemeProvider";
import { palette, radius, space, type } from "../theme/tokens";
import type { Box, CategoryOption, ItemDraft, PendingCapture, PendingState, QueuedEntry, Recommendation } from "../types";
import { DestinationBlock } from "../components/DestinationBlock";
import { FillCheck } from "../components/FillCheck";
import { ItemCard } from "../components/ItemCard";
import type { Thumbnail } from "../components/ItemCard";
import { Button } from "../ui/controls";
import { Icon } from "../ui/Icon";
import { Card, Group, ListRow, Notice } from "../ui/surfaces";
import { Header, Screen } from "../ui/Screen";

type Back = { label: string; onPress: () => void; disabled?: boolean };

export function WaitingScreen(props: {
  photo: PersistedPhoto | null;
  error: string;
  onRetry: () => void;
  onTakeAnother: () => void;
}) {
  return (
    <Screen
      footer={<Button label="Take another" onPress={props.onTakeAnother} />}
    >
      <Header
        title="Looking at your photo"
        subtitle="About five seconds for each thing in the picture. You don’t have to wait: it carries on while you take the next one."
      />
      {props.photo && <Image source={{ uri: props.photo.uri }} style={styles.photo} />}
      {props.error !== "" ? (
        /* Recorded on disk and, until it was shown here, seen nowhere:
           somebody in a storage unit with no signal watched a spinner that
           would never resolve. */
        <Notice
          tone="warn"
          title="Still trying"
          action={<Button label="Try again now" kind="secondary" compact onPress={props.onRetry} />}
        >
          {props.error}
        </Notice>
      ) : (
        <ActivityIndicator size="large" style={styles.spinner} />
      )}
    </Screen>
  );
}

export function InboxScreen(props: {
  back: Back;
  pending: PendingState;
  onOpen: (capture: PendingCapture) => void;
  onRetry: (capture: PendingCapture) => void;
  onDiscard: (capture: PendingCapture) => void;
}) {
  const captures = props.pending.captures;
  return (
    <Screen>
      <Header back={props.back} title="Photos waiting" subtitle={captures.length === 0 ? "Nothing waiting. Every photo you took has been reviewed." : "Review each when it’s ready. Nothing is filed until you do."} />
      {captures.map((capture) => {
        const ready = capture.status === "ready";
        const failed = capture.status === "failed";
        const count = capture.items?.length ?? 0;
        return (
          <Card key={capture.captureId}>
            <View style={styles.inboxHead}>
              <Image source={{ uri: storedPhotoUri(capture.photoName) }} style={styles.inboxThumb} />
              <View style={styles.grow}>
                <Text style={type.title}>
                  {ready
                    ? `${count} ${plural(count, "item", "items")} found`
                    : failed
                      ? "Could not identify"
                      : props.pending.working === capture.captureId
                        ? "Identifying…"
                        : "Waiting"}
                </Text>
                <Text style={[type.label, capture.error ? styles.warnText : null]}>
                  {capture.error ? capture.error : ago(capture.queuedAt)}
                </Text>
              </View>
            </View>
            <View style={styles.actions}>
              {ready && <Button label="Review" compact onPress={() => props.onOpen(capture)} />}
              {/* Offered for a stalled capture as well as a failed one: a
                  retriable failure leaves it "identifying" with the reason
                  recorded, and nothing else here would get it moving. */}
              {(failed || (!ready && (capture.error ?? "") !== "")) && (
                <Button label="Try again" kind="secondary" compact onPress={() => props.onRetry(capture)} />
              )}
              <Button label="Discard" kind="danger" compact accessibilityLabel="Discard this photo" onPress={() => props.onDiscard(capture)} />
            </View>
          </Card>
        );
      })}
    </Screen>
  );
}

export function ReviewScreen(props: {
  back: Back;
  busy: boolean;
  photo: PersistedPhoto | null;
  drafts: Draft[];
  expandedId: string;
  categories: CategoryOption[];
  categoriesLive: boolean;
  thumbnailFor: (region: ItemDraft["region"]) => Thumbnail | undefined;
  onToggle: (id: string) => void;
  onRemove: (draft: Draft, index: number) => void;
  onChange: (id: string, patch: Partial<ItemDraft>) => void;
  onCategory: (id: string, key: string, label: string) => void;
  onAdd: () => void;
  onNext: () => void;
}) {
  const single = props.drafts.length === 1;
  const incomplete = firstIncomplete(props.drafts) !== null;
  return (
    <Screen
      footer={
        /*
          Category is required, not defaulted. It is the engine's dominant
          signal and it becomes a tag in Homebox, so filling it in for the user
          would be both a worse recommendation and a tag they never chose. The
          button says which item is missing what.

          With a list, an incomplete item can be collapsed and off screen, so
          the button stays tappable and opens it. With one item there is
          nothing to open, so it is simply disabled.
        */
        <Button
          label={reviewButtonLabel(props.drafts)}
          busy={props.busy}
          disabled={props.drafts.length === 0 || (single && incomplete)}
          onPress={props.onNext}
        />
      }
    >
      <Header
        back={props.back}
        title={single ? "Review this item" : `Review ${props.drafts.length} items`}
        subtitle={
          single
            ? "Check what was found and correct anything that’s wrong."
            : "Each is filed on its own. Tap one to correct it; remove anything that isn’t really there."
        }
      />
      {props.photo && <Image source={{ uri: props.photo.uri }} style={single ? styles.photo : styles.photoSmall} />}
      {props.photo && !props.photo.downscaled && (
        <Text style={type.caption}>Could not resize this photo. It will upload at full size.</Text>
      )}
      {props.drafts.map((draft, index) => (
        <ItemCard
          key={draft.id}
          draft={draft}
          index={index}
          // One item is the common case: no summary, no remove button -- just
          // the form.
          alone={single}
          expanded={single || props.expandedId === draft.id}
          categories={props.categories}
          categoriesLive={props.categoriesLive}
          thumbnail={props.thumbnailFor(draft.item.region)}
          onToggle={() => props.onToggle(draft.id)}
          onRemove={() => props.onRemove(draft, index)}
          onChange={(patch) => props.onChange(draft.id, patch)}
          onCategory={(key, label) => props.onCategory(draft.id, key, label)}
        />
      ))}
      <Button label="+ Add something it missed" kind="dashed" onPress={props.onAdd} />
    </Screen>
  );
}

export function RecommendScreen(props: {
  back: Back;
  busy: boolean;
  drafts: Draft[];
  recs: Recommendation[];
  stale: boolean;
  offlinePicks: Box[][] | null;
  choices: Record<string, Destination>;
  openDestId: string;
  categories: CategoryOption[];
  readyEntries: QueuedEntry[] | null;
  onToggle: (id: string) => void;
  onChoose: (draft: Draft, destination: Destination) => void;
  onSubmit: (entries: QueuedEntry[]) => void;
}) {
  const single = props.drafts.length === 1;
  const ready = props.readyEntries;
  return (
    <Screen
      footer={
        single ? undefined : (
          <Button
            label={ready ? `File ${props.drafts.length} items` : "Every item needs a place"}
            busy={props.busy}
            disabled={ready === null}
            onPress={() => {
              if (ready) props.onSubmit(ready);
            }}
          />
        )
      }
    >
      <Header
        back={props.back}
        title={single ? "Where it goes" : "Where they go"}
        subtitle={
          single
            ? "Tap a place to file it there."
            : ready
              ? "A place is picked for each. Change any you’d put somewhere else."
              : "Choose a place for the ones still marked."
        }
      />
      {props.stale && <Notice tone="caution">Showing saved places. Homebox could not be reached, so fill levels may be out of date.</Notice>}
      {props.offlinePicks !== null && (
        <Notice tone="caution" title="You’re offline">
          This is your saved list of places, not a recommendation. Pick one and the capture waits on this phone until the server can be reached.
        </Notice>
      )}
      {props.drafts.map((draft, index) => (
        <DestinationBlock
          key={draft.id}
          draft={draft}
          alone={single}
          busy={props.busy}
          chosen={props.choices[draft.id]}
          open={single || props.openDestId === draft.id}
          recommendation={props.recs[index]}
          picks={props.offlinePicks?.[index]}
          categories={props.categories}
          onToggle={() => props.onToggle(draft.id)}
          onChoose={(destination) => props.onChoose(draft, destination)}
        />
      ))}
    </Screen>
  );
}

export function DoneScreen(props: {
  captureId: string;
  filed: QueuedEntry[];
  failures: Failure[];
  queued: QueuedEntry[];
  photoWarning: string;
  checkBoxes: Box[];
  onKeepFailures: () => void;
  onLeave: () => void;
}) {
  const { accent, soft } = useTheme();
  const { filed, failures, queued } = props;
  const onlyQueued = filed.length === 0 && failures.length === 0 && queued.length > 0;
  const nothing = filed.length === 0 && failures.length > 0;
  const rows = onlyQueued ? queued : filed;
  return (
    <Screen footer={<Button label="Catalog something else" kind={failures.length > 0 ? "secondary" : "primary"} onPress={props.onLeave} />}>
      <View style={styles.doneHead}>
        <View style={[styles.doneMark, { backgroundColor: nothing ? palette.warnSoft : soft }]}>
          <Icon name={nothing ? "alert" : "check"} size={30} color={nothing ? palette.warn : accent} strokeWidth={2.4} />
        </View>
        <Text style={type.display} accessibilityRole="header">
          {doneTitle(filed, failures, queued)}
        </Text>
        <Text style={type.callout}>{doneSubtitle(filed, failures, queued)}</Text>
      </View>

      {rows.length > 0 && (
        <Group>
          {rows.map((entry, i) => (
            <ListRow
              key={`row-${i}`}
              title={`${quantityPrefix(entry.item)}${itemName(entry.item)}`}
              right={<Text style={type.label}>{entry.boxName}</Text>}
            />
          ))}
        </Group>
      )}

      {failures.length > 0 && (
        <Notice
          tone="warn"
          title={`Not filed. ${plural(failures.length, "This item is", "These items are")} only on this phone.`}
          action={
            <Button
              label={`Keep ${failures.length === 1 ? "it" : `all ${failures.length}`} to retry later`}
              compact
              onPress={props.onKeepFailures}
            />
          }
        >
          {failures.map((f) => `${itemName(f.entry.item)} → ${f.entry.boxName}: ${f.error}`).join("\n")}
        </Notice>
      )}

      {queued.length > 0 && filed.length > 0 && (
        <Notice tone="caution">
          {`${queued.length} ${plural(queued.length, "item is", "items are")} waiting on this phone and will file ${plural(queued.length, "itself", "themselves")} when the server is reachable.`}
        </Notice>
      )}

      <FillCheck boxes={props.checkBoxes} captureId={props.captureId} queued={queued} />

      {props.photoWarning !== "" && <Notice tone="caution">{props.photoWarning}</Notice>}
    </Screen>
  );
}

function doneTitle(filed: QueuedEntry[], failures: Failure[], queued: QueuedEntry[]): string {
  if (filed.length === 0 && failures.length === 0 && queued.length > 0) return "Saved on this phone";
  if (failures.length === 0) {
    const only = filed[0];
    return filed.length === 1 && only ? `Filed ${quantityPrefix(only.item)}${itemName(only.item)}` : `Filed ${filed.length} items`;
  }
  if (filed.length === 0) return "Nothing was filed";
  return `${filed.length} filed, ${failures.length} not`;
}

function doneSubtitle(filed: QueuedEntry[], failures: Failure[], queued: QueuedEntry[]): string {
  if (filed.length === 0 && failures.length === 0 && queued.length > 0) {
    return queued.length === 1
      ? `It files itself into ${queued[0]?.boxName ?? "its place"} once the server is reachable.`
      : `${queued.length} items file themselves once the server is reachable.`;
  }
  if (failures.length === 0) {
    const only = filed[0];
    return filed.length === 1 && only ? `Into ${only.boxName}, in Homebox.` : "In Homebox, each with the photo.";
  }
  if (filed.length === 0) return `${failures.length} ${plural(failures.length, "item", "items")} did not reach Homebox.`;
  return "The rest reached Homebox.";
}

const styles = StyleSheet.create({
  grow: { flex: 1, gap: 2 },
  photo: { width: "100%", height: 220, borderRadius: radius.lg, backgroundColor: palette.placeholder },
  photoSmall: { width: "100%", height: 140, borderRadius: radius.lg, backgroundColor: palette.placeholder },
  spinner: { marginTop: space.lg },
  inboxHead: { flexDirection: "row", alignItems: "center", gap: space.md },
  inboxThumb: { width: 56, height: 56, borderRadius: radius.md, backgroundColor: palette.placeholder },
  actions: { flexDirection: "row", flexWrap: "wrap", gap: space.sm },
  warnText: { color: palette.warn },
  doneHead: { gap: 10, marginTop: space.xl },
  doneMark: { width: 56, height: 56, borderRadius: 28, alignItems: "center", justifyContent: "center" },
});
