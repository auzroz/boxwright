import type { ReactNode } from "react";
import { Pressable, StyleSheet, Text, View } from "react-native";
import { SafeAreaInsetsContext, useSafeAreaInsets } from "react-native-safe-area-context";

import { palette, space } from "../theme/tokens";

/**
 * A strip across the top of every screen while the demo is on, so nobody can
 * mistake sample data for their own inventory, with the way out always in
 * reach. The screen below is told the strip has taken the top inset.
 */
export function DemoBanner(props: { onLeave: () => void; children: ReactNode }) {
  const insets = useSafeAreaInsets();
  return (
    <View style={styles.root}>
      <View style={[styles.strip, { paddingTop: insets.top + 4 }]} accessibilityRole="summary">
        <Text style={styles.badge}>DEMO</Text>
        <Text style={styles.text} numberOfLines={2}>
          Sample inventory. Nothing is saved or sent.
        </Text>
        <Pressable
          accessibilityRole="button"
          accessibilityLabel="Leave the demo"
          onPress={props.onLeave}
          style={styles.leave}
          hitSlop={6}
        >
          <Text style={styles.leaveText}>Leave</Text>
        </Pressable>
      </View>
      <SafeAreaInsetsContext.Provider value={{ ...insets, top: 0 }}>{props.children}</SafeAreaInsetsContext.Provider>
    </View>
  );
}

const styles = StyleSheet.create({
  root: { flex: 1 },
  strip: {
    backgroundColor: palette.ink,
    flexDirection: "row",
    alignItems: "center",
    gap: space.sm,
    paddingHorizontal: space.lg,
    paddingBottom: 6,
  },
  badge: {
    color: palette.ink,
    backgroundColor: palette.ground,
    fontSize: 11,
    fontWeight: "700",
    letterSpacing: 0.8,
    paddingHorizontal: 6,
    paddingVertical: 2,
    borderRadius: 4,
    overflow: "hidden",
  },
  text: { flex: 1, color: palette.onAccent, fontSize: 14 },
  leave: { minHeight: 44, justifyContent: "center", paddingHorizontal: space.sm },
  leaveText: { color: palette.onAccent, fontSize: 15, fontWeight: "600", textDecorationLine: "underline" },
});
