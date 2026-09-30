import { Children, Fragment, isValidElement } from "react";
import type { ReactNode } from "react";
import { Pressable, StyleSheet, Text, TextInput, View } from "react-native";
import type { KeyboardTypeOptions, StyleProp, TextInputProps, ViewStyle } from "react-native";

import { useTheme } from "../theme/ThemeProvider";
import { MIN_TARGET, palette, radius, space, type } from "../theme/tokens";
import { Icon } from "./Icon";
import type { IconName } from "./Icon";

/** A white card on the paper ground. `highlight` outlines it in the accent. */
export function Card(props: { children: ReactNode; highlight?: boolean; style?: StyleProp<ViewStyle> }) {
  const { accent } = useTheme();
  return (
    <View style={[styles.card, props.highlight && { borderColor: accent, borderWidth: 1.5 }, props.style]}>
      {props.children}
    </View>
  );
}

/**
 * Rows in one card with hairlines between them, the iOS grouped-list shape.
 * `inset` is where the hairline starts, so it lines up with the row's text.
 */
export function Group(props: { children: ReactNode; inset?: number }) {
  const rows = Children.toArray(props.children).filter(isValidElement);
  return (
    <View style={[styles.card, styles.group]}>
      {rows.map((row, i) => (
        <Fragment key={row.key ?? i}>
          {i > 0 ? <View style={[styles.hairline, { marginLeft: props.inset ?? space.lg }]} /> : null}
          {row}
        </Fragment>
      ))}
    </View>
  );
}

/** One tappable row: an icon or picture, a title and a line under it, and what is on the right. */
export function ListRow(props: {
  title: string;
  detail?: string;
  detailTone?: "muted" | "warn";
  icon?: IconName;
  leading?: ReactNode;
  right?: ReactNode;
  chevron?: boolean;
  onPress?: () => void;
  disabled?: boolean;
  accessibilityLabel?: string;
}) {
  const { accent } = useTheme();
  const content = (
    <>
      {props.icon ? <Icon name={props.icon} color={accent} /> : null}
      {props.leading}
      <View style={styles.rowText}>
        <Text style={props.detail ? type.title : type.body}>{props.title}</Text>
        {props.detail ? (
          <Text style={[type.label, props.detailTone === "warn" && styles.warnText]}>{props.detail}</Text>
        ) : null}
      </View>
      {props.right}
      {props.chevron ? <Icon name="forward" size={18} color={palette.faint} strokeWidth={2} /> : null}
    </>
  );
  if (!props.onPress) return <View style={styles.row}>{content}</View>;
  return (
    <Pressable
      style={({ pressed }) => [styles.row, pressed && styles.pressed, props.disabled && styles.disabled]}
      accessibilityRole="button"
      accessibilityLabel={props.accessibilityLabel}
      disabled={props.disabled}
      onPress={props.onPress}
    >
      {content}
    </Pressable>
  );
}

type Tone = "info" | "caution" | "warn" | "ok";

/**
 * Something the user should know about this screen: offline, a failure, a
 * partial save. One plain sentence, and the action that deals with it.
 */
export function Notice(props: { tone?: Tone; title?: string; children?: ReactNode; action?: ReactNode }) {
  const tone = props.tone ?? "info";
  const colours = TONES[tone];
  return (
    <View style={[styles.notice, { backgroundColor: colours.bg, borderColor: colours.border }]} accessibilityRole={tone === "warn" ? "alert" : undefined}>
      {props.title ? <Text style={[styles.noticeTitle, { color: colours.fg }]}>{props.title}</Text> : null}
      {typeof props.children === "string" ? (
        <Text style={[styles.noticeText, { color: colours.fg }]}>{props.children}</Text>
      ) : (
        props.children
      )}
      {props.action}
    </View>
  );
}

const TONES: Record<Tone, { bg: string; border: string; fg: string }> = {
  info: { bg: palette.surface, border: palette.line, fg: palette.muted },
  caution: { bg: palette.cautionSoft, border: "#EBD9B4", fg: palette.caution },
  warn: { bg: palette.warnSoft, border: "#E8C3BA", fg: palette.warn },
  ok: { bg: palette.okSoft, border: "#C7DCC5", fg: palette.ok },
};

/** A small fact about a thing: its category, its size, "Fragile". */
export function Tag(props: { label: string; tone?: "plain" | "caution" | "warn" | "accent" }) {
  const { accent, soft } = useTheme();
  const tone = props.tone ?? "plain";
  const style =
    tone === "caution"
      ? { backgroundColor: palette.cautionSoft, color: palette.caution }
      : tone === "warn"
        ? { backgroundColor: palette.warnSoft, color: palette.warn }
        : tone === "accent"
          ? { backgroundColor: soft, color: accent }
          : { backgroundColor: palette.ground, color: palette.ink };
  return (
    <Text style={[styles.tag, { backgroundColor: style.backgroundColor, color: style.color }, tone !== "plain" && styles.tagStrong]}>
      {props.label}
    </Text>
  );
}

export function TagRow(props: { children: ReactNode }) {
  return <View style={styles.tagRow}>{props.children}</View>;
}

/** The small capitals that divide a long card into parts. */
export function SectionLabel(props: { children: string }) {
  return <Text style={type.section}>{props.children.toUpperCase()}</Text>;
}

export function Divider() {
  return <View style={styles.hairline} />;
}

/** A labelled text input. `right` sits beside the label: a unit switch, say. */
export function TextField(props: {
  label: string;
  value: string;
  onChange: (v: string) => void;
  placeholder?: string;
  error?: string;
  hint?: string;
  right?: ReactNode;
  keyboardType?: KeyboardTypeOptions;
  secret?: boolean;
  /** Addresses and tokens: no capitals, no autocorrect, no spellcheck. */
  literal?: boolean;
  autoCapitalize?: TextInputProps["autoCapitalize"];
  textContentType?: TextInputProps["textContentType"];
  returnKeyType?: TextInputProps["returnKeyType"];
  /** When editing ends by any route: blur or return. */
  onEndEditing?: () => void;
  onFocus?: () => void;
  /** Return only. */
  onSubmit?: () => void;
  autoFocus?: boolean;
  accessibilityLabel?: string;
}) {
  const literal = props.literal === true;
  return (
    <View style={styles.field}>
      <View style={styles.fieldHead}>
        <Text style={[type.label, styles.fieldLabel]}>{props.label}</Text>
        {props.right}
      </View>
      <TextInput
        style={[styles.input, props.error !== undefined && styles.inputError]}
        value={props.value}
        onChangeText={props.onChange}
        placeholder={props.placeholder}
        placeholderTextColor={palette.faint}
        keyboardType={props.keyboardType}
        secureTextEntry={props.secret}
        autoCapitalize={literal ? "none" : props.autoCapitalize}
        autoCorrect={literal ? false : undefined}
        spellCheck={literal ? false : undefined}
        textContentType={props.textContentType}
        returnKeyType={props.returnKeyType}
        onEndEditing={props.onEndEditing}
        onFocus={props.onFocus}
        onSubmitEditing={props.onSubmit ?? props.onEndEditing}
        autoFocus={props.autoFocus}
        accessibilityLabel={props.accessibilityLabel ?? props.label}
      />
      {props.error !== undefined ? <Text style={[type.caption, styles.warnText]}>{props.error}</Text> : null}
      {props.hint !== undefined && props.error === undefined ? <Text style={type.caption}>{props.hint}</Text> : null}
    </View>
  );
}

const styles = StyleSheet.create({
  card: {
    backgroundColor: palette.surface,
    borderWidth: 1,
    borderColor: palette.line,
    borderRadius: radius.xl,
    padding: space.lg,
    gap: space.md,
  },
  group: { padding: 0, gap: 0, overflow: "hidden" },
  hairline: { height: StyleSheet.hairlineWidth, backgroundColor: palette.line },
  row: {
    minHeight: 56,
    paddingHorizontal: space.lg,
    paddingVertical: space.sm,
    flexDirection: "row",
    alignItems: "center",
    gap: 14,
  },
  rowText: { flex: 1, gap: 2 },
  pressed: { backgroundColor: palette.hairline },
  disabled: { opacity: 0.4 },
  warnText: { color: palette.warn },
  notice: { borderWidth: 1, borderRadius: radius.lg, padding: 14, gap: space.sm },
  noticeTitle: { fontSize: 16, fontWeight: "600" },
  noticeText: { fontSize: 15, lineHeight: 21 },
  tag: { fontSize: 13, paddingHorizontal: 8, paddingVertical: 3, borderRadius: 8, overflow: "hidden" },
  tagStrong: { fontWeight: "600" },
  tagRow: { flexDirection: "row", flexWrap: "wrap", gap: 6 },
  field: { gap: 6 },
  fieldHead: { flexDirection: "row", alignItems: "center", gap: space.sm },
  fieldLabel: { flex: 1 },
  input: {
    minHeight: MIN_TARGET,
    borderWidth: 1,
    borderColor: palette.line,
    borderRadius: radius.md,
    paddingHorizontal: space.md,
    paddingVertical: 10,
    fontSize: 17,
    color: palette.ink,
    backgroundColor: palette.surface,
  },
  inputError: { borderColor: palette.warn },
});
