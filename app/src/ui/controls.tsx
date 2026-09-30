import type { ReactNode } from "react";
import { ActivityIndicator, Pressable, StyleSheet, Text, View } from "react-native";

import { useTheme } from "../theme/ThemeProvider";
import { MIN_TARGET, palette, radius, space, type } from "../theme/tokens";
import { Icon } from "./Icon";
import type { IconName } from "./Icon";

type ButtonKind = "primary" | "secondary" | "quiet" | "danger" | "dashed";

/**
 * The one button. Primary is the action that moves the screen on and there is
 * at most one; secondary is an alternative to it; quiet is a text action.
 */
export function Button(props: {
  label: string;
  onPress: () => void;
  kind?: ButtonKind;
  disabled?: boolean;
  busy?: boolean;
  icon?: IconName;
  accessibilityLabel?: string;
  compact?: boolean;
}) {
  const { accent } = useTheme();
  const kind = props.kind ?? "primary";
  const off = props.disabled === true || props.busy === true;
  const fg =
    kind === "primary"
      ? palette.onAccent
      : kind === "danger"
        ? palette.warn
        : kind === "quiet"
          ? accent
          : kind === "dashed"
            ? palette.muted
            : palette.ink;
  return (
    <Pressable
      accessibilityRole="button"
      accessibilityLabel={props.accessibilityLabel ?? props.label}
      accessibilityState={{ disabled: off, busy: props.busy === true }}
      disabled={off}
      onPress={props.onPress}
      style={({ pressed }) => [
        styles.button,
        props.compact && styles.compact,
        kind === "primary" && { backgroundColor: accent },
        kind === "secondary" && styles.secondary,
        kind === "danger" && styles.dangerButton,
        kind === "quiet" && styles.quiet,
        kind === "dashed" && styles.dashedButton,
        pressed && styles.pressed,
        off && styles.disabled,
      ]}
    >
      {props.busy ? (
        <ActivityIndicator color={fg} />
      ) : (
        <>
          {props.icon ? <Icon name={props.icon} size={20} color={kind === "secondary" ? accent : fg} /> : null}
          <Text style={[styles.buttonText, { color: fg }, kind === "quiet" && styles.quietText]}>{props.label}</Text>
        </>
      )}
    </Pressable>
  );
}

/** A choice among a few, shown as pills. `onPress` toggles; the parent owns the value. */
export function Chip(props: {
  label: string;
  selected?: boolean;
  onPress: () => void;
  dashed?: boolean;
  /** A category Homebox does not have yet: filing it creates a tag. */
  fresh?: boolean;
  accessibilityLabel?: string;
}) {
  const { accent, soft } = useTheme();
  const on = props.selected === true;
  return (
    <Pressable
      accessibilityRole="button"
      accessibilityState={{ selected: on }}
      accessibilityLabel={props.accessibilityLabel ?? props.label}
      onPress={props.onPress}
      hitSlop={{ top: 2, bottom: 2 }}
      style={[
        styles.chip,
        props.dashed && styles.chipDashed,
        props.fresh && !on && { borderStyle: "dashed", borderColor: accent, backgroundColor: soft },
        on && { backgroundColor: accent, borderColor: accent },
      ]}
    >
      <Text style={[styles.chipText, props.dashed && styles.chipDashedText, on && styles.chipTextOn]}>{props.label}</Text>
    </Pressable>
  );
}

export function ChipRow(props: { children: ReactNode }) {
  return <View style={styles.chipRow}>{props.children}</View>;
}

/**
 * Two to five mutually exclusive options in one control: weight, access,
 * units. A value of undefined selects nothing, for an optional answer.
 */
export function Segmented<T extends string>(props: {
  options: readonly { value: T; label: string }[];
  value: T | undefined;
  onChange: (value: T) => void;
  accessibilityLabel: string;
  small?: boolean;
}) {
  return (
    <View style={[styles.segmented, props.small && styles.segmentedSmall]} accessibilityRole="radiogroup" accessibilityLabel={props.accessibilityLabel}>
      {props.options.map((o) => {
        const on = o.value === props.value;
        return (
          <Pressable
            key={o.value}
            accessibilityRole="radio"
            accessibilityState={{ checked: on }}
            accessibilityLabel={o.label}
            onPress={() => props.onChange(o.value)}
            style={[styles.segment, props.small && styles.segmentSmall, on && styles.segmentOn]}
          >
            <Text style={[styles.segmentText, on && styles.segmentTextOn]}>{o.label}</Text>
          </Pressable>
        );
      })}
    </View>
  );
}

/** A labelled on/off row, for inside a grouped list. */
export function ToggleRow(props: { label: string; value: boolean; onChange: (v: boolean) => void; detail?: string }) {
  return (
    <View style={styles.row}>
      <View style={styles.rowText}>
        <Text style={type.body}>{props.label}</Text>
        {props.detail ? <Text style={type.caption}>{props.detail}</Text> : null}
      </View>
      <Toggle value={props.value} onChange={props.onChange} label={props.label} />
    </View>
  );
}

/**
 * An on/off switch, drawn. The system Switch is laid out at the top of its row
 * under the new architecture rather than centred in it, which put every one
 * of them half out of its row.
 */
export function Toggle(props: { value: boolean; onChange: (v: boolean) => void; label: string }) {
  const { accent } = useTheme();
  return (
    <Pressable
      accessibilityRole="switch"
      accessibilityState={{ checked: props.value }}
      accessibilityLabel={props.label}
      onPress={() => props.onChange(!props.value)}
      hitSlop={8}
      style={[styles.track, { backgroundColor: props.value ? accent : palette.line }]}
    >
      <View style={[styles.knob, props.value && styles.knobOn]} />
    </Pressable>
  );
}

/** How many, with a floor of one. */
export function Stepper(props: { label: string; value: number; onChange: (n: number) => void }) {
  return (
    <View style={styles.row}>
      <Text style={[type.body, styles.rowText]}>{props.label}</Text>
      <View style={styles.stepper}>
        <Pressable
          style={styles.stepperButton}
          accessibilityRole="button"
          accessibilityLabel="One fewer"
          disabled={props.value <= 1}
          onPress={() => props.onChange(Math.max(1, props.value - 1))}
        >
          <Text style={[styles.stepperGlyph, props.value <= 1 && styles.disabled]}>−</Text>
        </Pressable>
        <Text style={styles.stepperValue} accessibilityLabel={`${props.value}`}>
          {props.value}
        </Text>
        <Pressable
          style={styles.stepperButton}
          accessibilityRole="button"
          accessibilityLabel="One more"
          onPress={() => props.onChange(props.value + 1)}
        >
          <Text style={styles.stepperGlyph}>+</Text>
        </Pressable>
      </View>
    </View>
  );
}

/** The square tick of a multi-select list. Drawn, not a glyph, so it matches the radio. */
export function Checkbox(props: { checked: boolean }) {
  const { accent } = useTheme();
  return props.checked ? (
    <View style={[styles.box, { backgroundColor: accent, borderColor: accent }]}>
      <Icon name="check" size={15} color={palette.onAccent} strokeWidth={3} />
    </View>
  ) : (
    <View style={styles.box} />
  );
}

/** The round mark of a pick-one list. */
export function Radio(props: { checked: boolean }) {
  const { accent } = useTheme();
  return props.checked ? (
    <View style={[styles.radio, { backgroundColor: accent, borderColor: accent }]}>
      <Icon name="check" size={14} color={palette.onAccent} strokeWidth={3} />
    </View>
  ) : (
    <View style={styles.radio} />
  );
}

const styles = StyleSheet.create({
  button: {
    minHeight: 54,
    borderRadius: radius.lg,
    paddingHorizontal: space.lg,
    flexDirection: "row",
    alignItems: "center",
    justifyContent: "center",
    gap: space.sm,
  },
  compact: { minHeight: MIN_TARGET, borderRadius: radius.md },
  secondary: { backgroundColor: palette.surface, borderWidth: 1, borderColor: palette.line },
  dangerButton: { backgroundColor: palette.surface, borderWidth: 1, borderColor: palette.warnSoft },
  quiet: { minHeight: MIN_TARGET, backgroundColor: "transparent" },
  dashedButton: { minHeight: 52, borderWidth: 1.5, borderStyle: "dashed", borderColor: palette.control },
  buttonText: { fontSize: 17, fontWeight: "600" },
  quietText: { fontSize: 16 },
  pressed: { opacity: 0.8 },
  disabled: { opacity: 0.4 },
  chipRow: { flexDirection: "row", flexWrap: "wrap", gap: space.sm },
  chip: {
    minHeight: 40,
    paddingHorizontal: space.lg,
    borderRadius: 20,
    borderWidth: 1,
    borderColor: palette.line,
    backgroundColor: palette.surface,
    justifyContent: "center",
  },
  chipDashed: { borderStyle: "dashed", borderColor: palette.control, backgroundColor: "transparent" },
  chipText: { fontSize: 15, color: palette.ink },
  chipDashedText: { color: palette.muted },
  chipTextOn: { color: palette.onAccent, fontWeight: "600" },
  segmented: { flexDirection: "row", backgroundColor: palette.hairline, borderRadius: radius.md, padding: 3 },
  segmentedSmall: { borderRadius: 10, padding: 2 },
  segment: { flex: 1, minHeight: 38, borderRadius: 9, alignItems: "center", justifyContent: "center", paddingHorizontal: space.sm },
  segmentSmall: { flex: 0, minHeight: 32, minWidth: 44, borderRadius: 8 },
  segmentOn: { backgroundColor: palette.surface },
  segmentText: { fontSize: 15, color: palette.muted },
  segmentTextOn: { color: palette.ink, fontWeight: "600" },
  track: { width: 51, height: 31, borderRadius: 16, padding: 2, justifyContent: "center" },
  knob: {
    width: 27,
    height: 27,
    borderRadius: 14,
    backgroundColor: palette.surface,
    shadowColor: "#000",
    shadowOpacity: 0.15,
    shadowRadius: 2,
    shadowOffset: { width: 0, height: 1 },
  },
  knobOn: { alignSelf: "flex-end" },
  row: { minHeight: 52, paddingHorizontal: space.md, paddingVertical: space.sm, flexDirection: "row", alignItems: "center", gap: space.md },
  rowText: { flex: 1, gap: 2 },
  stepper: { flexDirection: "row", alignItems: "center", gap: space.xs },
  stepperButton: {
    width: MIN_TARGET,
    height: MIN_TARGET,
    borderRadius: MIN_TARGET / 2,
    borderWidth: 1,
    borderColor: palette.line,
    backgroundColor: palette.surface,
    alignItems: "center",
    justifyContent: "center",
  },
  stepperGlyph: { fontSize: 20, color: palette.ink },
  stepperValue: { minWidth: 28, textAlign: "center", fontSize: 17, fontWeight: "600", color: palette.ink },
  box: {
    width: 24,
    height: 24,
    borderRadius: 7,
    borderWidth: 1.5,
    borderColor: palette.control,
    alignItems: "center",
    justifyContent: "center",
  },
  radio: {
    width: 22,
    height: 22,
    borderRadius: 11,
    borderWidth: 1.5,
    borderColor: palette.control,
    alignItems: "center",
    justifyContent: "center",
  },
});
