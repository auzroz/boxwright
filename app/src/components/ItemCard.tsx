import { Image, Pressable, StyleSheet, Text, View } from "react-native";

import { normalizeDims } from "../capacity";
import { categoryLabel } from "../categories";
import { coverCrop } from "../crop";
import type { CropRect } from "../crop";
import { SIZE_WORDS, quantityPrefix } from "../draft";
import type { Draft } from "../draft";
import { useTheme } from "../theme/ThemeProvider";
import { MIN_TARGET, palette, radius, space, type } from "../theme/tokens";
import type { CategoryOption, ItemDraft, SizeBucket, WeightClass } from "../types";
import { Segmented, Stepper, ToggleRow } from "../ui/controls";
import { Icon } from "../ui/Icon";
import { Card, Divider, Group, SectionLabel, Tag, TagRow, TextField } from "../ui/surfaces";
import { formatDimsIn, useUnits } from "../units";
import type { UnitSystem } from "../units";
import { CategoryPicker } from "./CategoryPicker";
import { SizeField } from "./SizeField";

/** One item's part of the capture photo, drawn as a clipped view of it. */
export type Thumbnail = { uri: string; crop: CropRect; photo: { width: number; height: number } };

/** The per-item thumbnail's side, in points. coverCrop fills exactly this. */
export const ITEM_THUMB_SIDE = 52;

/** Below this the card says to check what the model said. */
const UNSURE = 0.8;

const WEIGHTS = [
  { value: "light", label: "Light" },
  { value: "medium", label: "Medium" },
  { value: "heavy", label: "Heavy" },
] as const;

const BUCKETS = (["S", "M", "L", "XL"] as const).map((value) => ({ value, label: value === "XL" ? "X-large" : SIZE_WORDS[value] }));

/**
 * One item on the review screen: a summary card that opens into the full form.
 *
 * `alone` collapses the apparatus away. A photo of one thing is the common
 * case and it deserves the form on its own -- no summary to tap past, no remove
 * button beside it, nothing to serve the eight-item case at its expense.
 */
export function ItemCard(props: {
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
  const item = props.draft.item;
  const system = useUnits();
  const name = item.name.trim() || `Item ${props.index + 1}`;

  if (!props.expanded) {
    return (
      <Pressable
        accessibilityRole="button"
        accessibilityState={{ expanded: false }}
        accessibilityLabel={`${quantityPrefix(item)}${name}. Edit`}
        onPress={props.onToggle}
        style={({ pressed }) => [styles.collapsed, pressed && styles.pressed]}
      >
        <Thumb thumbnail={props.thumbnail} />
        <View style={styles.grow}>
          <Text style={type.title}>
            {quantityPrefix(item)}
            {name}
          </Text>
          <SummaryTags item={item} categories={props.categories} system={system} />
        </View>
        <Icon name="forward" size={18} color={palette.faint} strokeWidth={2} />
      </Pressable>
    );
  }

  return (
    <Card highlight={!props.alone} style={styles.open}>
      {!props.alone && (
        <View style={styles.head}>
          <Thumb thumbnail={props.thumbnail} />
          <Pressable
            style={styles.headText}
            accessibilityRole="button"
            accessibilityState={{ expanded: true }}
            accessibilityLabel={`${name}. Collapse`}
            onPress={props.onToggle}
          >
            <Text style={type.title}>
              {quantityPrefix(item)}
              {name}
            </Text>
            <Text style={type.label}>{confidenceLine(item)}</Text>
          </Pressable>
          {/*
            Apart from the header that collapses the card, with its own target:
            the two live side by side and only one of them is recoverable from.
          */}
          <Pressable
            style={styles.remove}
            accessibilityRole="button"
            accessibilityLabel={`Remove ${name}`}
            onPress={props.onRemove}
          >
            <Icon name="trash" size={20} color={palette.warn} />
          </Pressable>
        </View>
      )}
      {props.alone && item.confidence > 0 && <Text style={type.label}>{confidenceLine(item)}</Text>}

      <SectionLabel>What it is</SectionLabel>
      <TextField label="Name" value={item.name} onChange={(v) => props.onChange({ name: v })} autoCapitalize="sentences" />
      <CategoryPicker
        options={props.categories}
        live={props.categoriesLive}
        value={item.category}
        proposed={props.draft.proposed && props.draft.proposed.key === item.category ? props.draft.proposed : null}
        onChange={props.onCategory}
      />
      <TextField label="Notes" value={item.notes} onChange={(v) => props.onChange({ notes: v })} autoCapitalize="sentences" />

      <Divider />
      <SectionLabel>Size and handling</SectionLabel>
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
      {/*
        The bucket is what the engine uses when there is no size, so it is only
        asked for then. With a size it would be a second answer to the same
        question, and the size wins.
      */}
      {!item.bulky && !normalizeDims(item.dimensionsCm) && (
        <View style={styles.field}>
          <Text style={type.label}>Roughly how big</Text>
          <Segmented<SizeBucket>
            options={BUCKETS}
            value={item.sizeBucket}
            onChange={(sizeBucket) => props.onChange({ sizeBucket })}
            accessibilityLabel="Roughly how big"
          />
        </View>
      )}
      <View style={styles.field}>
        <Text style={type.label}>Weight</Text>
        <Segmented<WeightClass>
          options={WEIGHTS}
          value={item.weightClass}
          onChange={(weightClass) => props.onChange({ weightClass })}
          accessibilityLabel="Weight"
        />
      </View>
      <Group inset={space.md}>
        <ToggleRow label="Fragile" value={item.fragile} onChange={(fragile) => props.onChange({ fragile })} />
        {/*
          Correctable, because it decides between two completely different
          answers: a container, or somewhere to stand it. Sizes stop meaning
          anything once this is on.
        */}
        <ToggleRow
          label="Too big for a box"
          detail={item.bulky ? "It will be offered somewhere to stand, not a container." : undefined}
          value={item.bulky}
          onChange={(bulky) => props.onChange({ bulky })}
        />
        <Stepper label="How many" value={item.quantity} onChange={(quantity) => props.onChange({ quantity })} />
      </Group>
    </Card>
  );
}

function confidenceLine(item: ItemDraft): string {
  if (item.confidence <= 0) return "Added by hand";
  const pct = Math.round(item.confidence * 100);
  return item.confidence < UNSURE ? `${pct}% sure. Check it below.` : `${pct}% sure. Edit anything below.`;
}

/** The short facts on a collapsed card, and what still needs doing. */
function SummaryTags(props: { item: ItemDraft; categories: CategoryOption[]; system: UnitSystem }) {
  const item = props.item;
  if (item.name.trim() === "") return <TagRow><Tag label="Needs a name" tone="caution" /></TagRow>;
  return (
    <TagRow>
      {item.category === "" ? (
        <Tag label="Needs a category" tone="caution" />
      ) : (
        <Tag label={categoryLabel(item.category, props.categories)} />
      )}
      <Tag label={item.bulky ? "Too big for a box" : sizePhrase(item, props.system)} />
      {item.fragile && <Tag label="Fragile" tone="warn" />}
      {item.confidence > 0 && item.confidence < UNSURE && (
        <Tag label={`${Math.round(item.confidence * 100)}% sure, check it`} tone="caution" />
      )}
    </TagRow>
  );
}

/** An item's size for a summary: measured, estimated, or its bucket. */
export function sizePhrase(item: ItemDraft, system: UnitSystem): string {
  const dims = normalizeDims(item.dimensionsCm);
  if (!dims) return SIZE_WORDS[item.sizeBucket];
  return item.dimensionsSource === "vision" ? `≈ ${formatDimsIn(dims, system)}` : formatDimsIn(dims, system);
}

/**
 * The picture of THIS thing, next to the row that names it. Eight items from
 * one shelf shot otherwise all carry the same photo of the shelf, which says
 * nothing about which row is which. Decorative: the text beside it already
 * says everything, so it is hidden from a screen reader.
 */
function Thumb(props: { thumbnail?: Thumbnail }) {
  const { soft } = useTheme();
  const t = props.thumbnail;
  return (
    <View
      style={[styles.thumb, { backgroundColor: t ? palette.placeholder : soft }]}
      accessibilityElementsHidden
      importantForAccessibility="no-hide-descendants"
    >
      {t ? (
        <Image source={{ uri: t.uri }} style={[styles.thumbImage, coverCrop(t.crop, t.photo, ITEM_THUMB_SIDE)]} />
      ) : (
        <Icon name="box" size={24} color={palette.faint} />
      )}
    </View>
  );
}

const styles = StyleSheet.create({
  collapsed: {
    backgroundColor: palette.surface,
    borderWidth: 1,
    borderColor: palette.line,
    borderRadius: radius.xl,
    paddingVertical: space.md,
    paddingHorizontal: 14,
    flexDirection: "row",
    alignItems: "center",
    gap: space.md,
  },
  pressed: { backgroundColor: palette.hairline },
  grow: { flex: 1, gap: 6 },
  open: { gap: 14, padding: 14 },
  head: { flexDirection: "row", alignItems: "center", gap: space.md },
  headText: { flex: 1, gap: 3, minHeight: MIN_TARGET, justifyContent: "center" },
  remove: {
    width: MIN_TARGET,
    height: MIN_TARGET,
    borderRadius: MIN_TARGET / 2,
    backgroundColor: palette.ground,
    alignItems: "center",
    justifyContent: "center",
  },
  field: { gap: space.sm },
  thumb: {
    width: ITEM_THUMB_SIDE,
    height: ITEM_THUMB_SIDE,
    borderRadius: radius.md,
    overflow: "hidden",
    alignItems: "center",
    justifyContent: "center",
  },
  thumbImage: { position: "absolute" },
});
