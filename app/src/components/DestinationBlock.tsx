import { Pressable, StyleSheet, Text, View } from "react-native";

import { knownFill } from "../capacity";
import { categoryLabel, normalizeCategory } from "../categories";
import { accessPhrase, newContainerDestination, quantityPrefix, sameDestination } from "../draft";
import type { Destination, Draft } from "../draft";
import { useTheme } from "../theme/ThemeProvider";
import { MIN_TARGET, palette, space, type } from "../theme/tokens";
import type { Box, CategoryOption, Recommendation } from "../types";
import { Radio } from "../ui/controls";
import { FillBar } from "../ui/FillBar";
import { Icon } from "../ui/Icon";
import { Card, Notice, Tag, TagRow } from "../ui/surfaces";
import { formatVolume, useUnits } from "../units";

/** At or past this a known fill earns a "Nearly full" badge. */
const NEARLY_FULL = 75;

/**
 * Where one item is going, and the alternatives.
 *
 * Collapsed, it shows the place already picked, with how full it is and why it
 * was picked. Open, every option is a row in a pick-one list, the suggestion to
 * start a new container last and dashed. With one item the list is simply
 * open and tapping a place files it -- the common case, in one tap.
 */
export function DestinationBlock(props: {
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
  const title = `${quantityPrefix(item)}${item.name.trim()}`;

  if (!props.open) {
    const chosenBox = props.chosen?.kind === "box" ? findBox(props.chosen.boxId, candidates.map((c) => c.box), offline) : undefined;
    const reasons = withoutFill(
      props.chosen?.kind === "box" ? candidates.find((c) => c.box.id === chosenBox?.id)?.reasons ?? [] : [],
      chosenBox,
    );
    return (
      <Card>
        <View style={styles.cardHead}>
          <Text style={[type.title, styles.grow]}>{title}</Text>
          <ChangeButton label="Change" onPress={props.onToggle} disabled={props.busy} />
        </View>
        {props.chosen ? (
          <>
            <View style={styles.placeLine}>
              <Text style={styles.place}>{props.chosen.kind === "new" ? `New ${props.chosen.containerType ?? "container"}` : props.chosen.name}</Text>
              <Text style={[type.label, styles.grow]} numberOfLines={1}>
                {props.chosen.kind === "new" ? `“${props.chosen.name}”` : placeDetail(chosenBox)}
              </Text>
            </View>
            {chosenBox && !chosenBox.isArea && <FillBar box={chosenBox} />}
            {reasons.length > 0 && (
              <TagRow>
                {reasons.map((r) => (
                  <Tag key={r} label={sentence(r)} />
                ))}
              </TagRow>
            )}
          </>
        ) : (
          <Text style={[type.label, styles.warnText]}>No place for this one yet. Tap Change to choose.</Text>
        )}
      </Card>
    );
  }

  const options = (
    <View style={styles.options} accessibilityRole="radiogroup" accessibilityLabel={`Where ${title} goes`}>
      {candidates.map((candidate) => {
        const destination: Destination = { kind: "box", boxId: candidate.box.id, name: candidate.box.name };
        return (
          <PlaceOption
            key={candidate.box.id}
            box={candidate.box}
            detail={withoutFill(candidate.reasons ?? [], candidate.box).map(sentence).join(" · ")}
            selected={!props.alone && sameDestination(props.chosen, destination)}
            busy={props.busy}
            onPress={() => props.onChoose(destination)}
          />
        );
      })}

      {offline.map((box) => {
        const destination: Destination = { kind: "box", boxId: box.id, name: box.name };
        return (
          <PlaceOption
            key={box.id}
            box={box}
            detail={
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
        <NewContainerOption
          title={`New ${suggestion.containerType ?? `${sizeWord(suggestion.sizeBucket)} container`}`}
          detail={`${sentence(suggestion.reason)}. Labelled “${suggestion.label}”, somewhere ${accessPhrase(suggestion.access)}.`}
          capacityL={suggestion.capacityL}
          selected={!props.alone && props.chosen?.kind === "new"}
          busy={props.busy}
          onPress={() => props.onChoose(newContainerDestination(suggestion, props.recommendation))}
        />
      )}

      {noPlace && <Notice tone="caution" title="Too big for a container">{noPlace.reason}</Notice>}
      {empty && <Notice>No places to choose from. Choose where Boxwright may file things from the home screen.</Notice>}
    </View>
  );

  if (props.alone) return options;
  return (
    <Card highlight>
      <View style={styles.cardHead}>
        <Text style={[type.title, styles.grow]}>{title}</Text>
        <ChangeButton label="Done" onPress={props.onToggle} disabled={props.busy} />
      </View>
      {options}
    </Card>
  );
}

function ChangeButton(props: { label: string; onPress: () => void; disabled: boolean }) {
  const { accent } = useTheme();
  return (
    <Pressable
      accessibilityRole="button"
      onPress={props.onPress}
      disabled={props.disabled}
      style={[styles.change, props.disabled && styles.disabled]}
      hitSlop={6}
    >
      <Text style={[styles.changeText, { color: accent }]}>{props.label}</Text>
      {props.label === "Change" && <Icon name="forward" size={16} color={accent} strokeWidth={2} />}
    </Pressable>
  );
}

/** One place in the pick-one list: its name, where it is, how full, and why. */
function PlaceOption(props: { box: Box; detail: string; selected: boolean; busy: boolean; onPress: () => void }) {
  const { accent, soft } = useTheme();
  const fill = knownFill(props.box);
  return (
    <Pressable
      accessibilityRole="radio"
      accessibilityState={{ checked: props.selected, disabled: props.busy }}
      accessibilityLabel={`${props.box.name}${props.box.area ? `, in ${props.box.area}` : ""}`}
      disabled={props.busy}
      onPress={props.onPress}
      style={({ pressed }) => [
        styles.option,
        props.selected && { borderColor: accent, borderWidth: 2, backgroundColor: soft },
        pressed && !props.selected && styles.pressed,
        props.busy && styles.disabled,
      ]}
    >
      <View style={styles.optionHead}>
        <Radio checked={props.selected} />
        <Text style={[styles.optionName, styles.grow]}>{props.box.name}</Text>
        {fill !== undefined && fill >= NEARLY_FULL ? (
          <Tag label={fill >= 100 ? "Full" : "Nearly full"} tone="caution" />
        ) : props.box.area ? (
          <Text style={type.caption} numberOfLines={1}>
            {props.box.area}
          </Text>
        ) : null}
      </View>
      {!props.box.isArea && (
        <View style={styles.indent}>
          <FillBar box={props.box} />
        </View>
      )}
      {props.detail !== "" && <Text style={[type.caption, styles.indent]}>{props.detail}</Text>}
    </Pressable>
  );
}

function NewContainerOption(props: {
  title: string;
  detail: string;
  capacityL?: number;
  selected: boolean;
  busy: boolean;
  onPress: () => void;
}) {
  const { accent, soft } = useTheme();
  const system = useUnits();
  return (
    <Pressable
      accessibilityRole="radio"
      accessibilityState={{ checked: props.selected, disabled: props.busy }}
      accessibilityLabel={props.title}
      disabled={props.busy}
      onPress={props.onPress}
      style={({ pressed }) => [
        styles.option,
        styles.newOption,
        props.selected && { borderColor: accent, borderStyle: "solid", borderWidth: 2, backgroundColor: soft },
        pressed && !props.selected && styles.pressed,
        props.busy && styles.disabled,
      ]}
    >
      <View style={styles.optionHead}>
        <Radio checked={props.selected} />
        <Text style={[styles.optionName, styles.grow]}>{props.title}</Text>
        {props.capacityL ? <Text style={type.caption}>{formatVolume(props.capacityL, system)}</Text> : null}
      </View>
      <Text style={[type.caption, styles.indent]}>{props.detail}</Text>
    </Pressable>
  );
}

/**
 * The engine's reasons, minus the ones about fill wherever the fill bar
 * already says it -- "About 55% full" as a chip under a bar reading 55% full
 * is the same fact twice.
 */
function withoutFill(reasons: readonly string[], box: Box | undefined): string[] {
  if (!box || box.isArea) return [...reasons];
  return reasons.filter((r) => !/\b(full|fill)\b/i.test(r));
}

function findBox(id: string, ...lists: Box[][]): Box | undefined {
  for (const list of lists) {
    const found = list.find((b) => b.id === id);
    if (found) return found;
  }
  return undefined;
}

/** "Garage · 27-gallon tote": where it is and what it is, whichever are known. */
function placeDetail(box: Box | undefined): string {
  if (!box) return "";
  return [box.area, box.containerType ?? ""].filter((s) => s !== "").join(" · ");
}

function sizeWord(bucket: string): string {
  switch (bucket) {
    case "S":
      return "small";
    case "M":
      return "medium";
    case "L":
      return "large";
    default:
      return "extra-large";
  }
}

/** The engine's reasons are lower-case fragments; on their own they read as sentences. */
function sentence(s: string): string {
  const t = s.trim();
  return t === "" ? t : t[0]!.toUpperCase() + t.slice(1);
}

const styles = StyleSheet.create({
  grow: { flex: 1 },
  cardHead: { flexDirection: "row", alignItems: "center", gap: space.sm },
  change: { minHeight: MIN_TARGET, flexDirection: "row", alignItems: "center", gap: 2, paddingLeft: space.sm },
  changeText: { fontSize: 15, fontWeight: "600" },
  disabled: { opacity: 0.4 },
  placeLine: { flexDirection: "row", alignItems: "baseline", gap: space.sm },
  place: { fontSize: 20, fontWeight: "600", color: palette.ink },
  warnText: { color: palette.caution },
  options: { gap: space.sm },
  option: {
    borderWidth: 1,
    borderColor: palette.line,
    borderRadius: 14,
    backgroundColor: palette.surface,
    padding: space.md,
    gap: space.sm,
    minHeight: MIN_TARGET,
  },
  newOption: { borderStyle: "dashed", borderWidth: 1.5, borderColor: palette.control, backgroundColor: "transparent" },
  pressed: { backgroundColor: palette.hairline },
  optionHead: { flexDirection: "row", alignItems: "center", gap: 10 },
  optionName: { fontSize: 16, fontWeight: "600", color: palette.ink },
  indent: { paddingLeft: 32 },
});

