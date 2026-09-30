import type { ReactNode, Ref } from "react";
import { KeyboardAvoidingView, Pressable, ScrollView, StyleSheet, Text, View } from "react-native";
import { useSafeAreaInsets } from "react-native-safe-area-context";

import { useTheme } from "../theme/ThemeProvider";
import { MIN_TARGET, palette, space, type } from "../theme/tokens";
import { Icon } from "./Icon";

/**
 * One screen: the safe area, a scrolling body, and an optional footer that
 * stays put above the home indicator -- where the action that ends the screen
 * lives, so it is never scrolled out of reach.
 *
 * The keyboard pushes the footer up rather than covering it. A form whose
 * Save sits under the keyboard was found on a real phone, twice.
 */
export function Screen(props: {
  children: ReactNode;
  footer?: ReactNode;
  /** The footer is a panel of its own (a form), raised off the page rather than a strip of buttons. */
  sheet?: boolean;
  /** A screen that lays itself out (a centred message) rather than scrolling. */
  fixed?: boolean;
  scrollRef?: Ref<ScrollView>;
}) {
  const insets = useSafeAreaInsets();
  const top = insets.top + space.sm;
  const bottom = props.footer ? space.xxl : insets.bottom + space.xxl;
  return (
    <KeyboardAvoidingView style={styles.root} behavior="padding">
      {props.fixed ? (
        <View style={[styles.body, styles.fixed, { paddingTop: top, paddingBottom: bottom }]}>{props.children}</View>
      ) : (
        <ScrollView
          ref={props.scrollRef}
          contentContainerStyle={[styles.body, { paddingTop: top, paddingBottom: bottom }]}
          keyboardShouldPersistTaps="handled"
          keyboardDismissMode="interactive"
        >
          {props.children}
        </ScrollView>
      )}
      {/* Scrolled content passes under the status bar; this keeps the clock on paper. */}
      <View style={[styles.statusBar, { height: insets.top }]} pointerEvents="none" />
      {props.footer ? (
        <View style={[styles.footer, props.sheet && styles.sheet, { paddingBottom: Math.max(insets.bottom, space.lg) }]}>
          {props.footer}
        </View>
      ) : null}
    </KeyboardAvoidingView>
  );
}

/**
 * A screen's title, with the way back above it. The back label says where it
 * goes ("Items", "Start over") rather than being a bare chevron, because back
 * from different screens lands in different places.
 */
export function Header(props: {
  title: string;
  subtitle?: string;
  back?: { label: string; onPress: () => void; disabled?: boolean } | null;
  right?: ReactNode;
}) {
  const { accent } = useTheme();
  const back = props.back;
  return (
    <View style={styles.header}>
      {back ? (
        <Pressable
          style={styles.back}
          accessibilityRole="button"
          accessibilityLabel={back.label}
          accessibilityState={{ disabled: back.disabled === true }}
          disabled={back.disabled}
          onPress={back.onPress}
          hitSlop={8}
        >
          <Icon name="back" size={20} color={accent} strokeWidth={2} />
          <Text style={[styles.backText, { color: accent }, back.disabled && styles.disabled]}>{back.label}</Text>
        </Pressable>
      ) : null}
      <View style={styles.titleRow}>
        <Text style={[type.display, styles.title]} accessibilityRole="header">
          {props.title}
        </Text>
        {props.right}
      </View>
      {props.subtitle ? <Text style={type.callout}>{props.subtitle}</Text> : null}
    </View>
  );
}

const styles = StyleSheet.create({
  root: { flex: 1, backgroundColor: palette.ground },
  statusBar: { position: "absolute", top: 0, left: 0, right: 0, backgroundColor: palette.ground, opacity: 0.96 },
  body: { paddingHorizontal: space.xl, gap: space.lg },
  fixed: { flex: 1 },
  footer: {
    paddingHorizontal: space.xl,
    paddingTop: space.md,
    gap: space.sm,
    backgroundColor: palette.ground,
    borderTopWidth: StyleSheet.hairlineWidth,
    borderTopColor: palette.line,
  },
  sheet: {
    backgroundColor: palette.surface,
    borderTopWidth: 0,
    borderTopLeftRadius: 24,
    borderTopRightRadius: 24,
    shadowColor: "#1E1B16",
    shadowOpacity: 0.12,
    shadowRadius: 20,
    shadowOffset: { width: 0, height: -6 },
  },
  header: { gap: space.xs },
  back: { alignSelf: "flex-start", minHeight: MIN_TARGET, flexDirection: "row", alignItems: "center", gap: 2 },
  backText: { fontSize: 17 },
  disabled: { opacity: 0.4 },
  titleRow: { flexDirection: "row", alignItems: "center", gap: space.md },
  title: { flex: 1 },
});
