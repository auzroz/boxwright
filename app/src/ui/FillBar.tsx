import { StyleSheet, Text, View } from "react-native";
import Svg, { Path } from "react-native-svg";

import { fillLabel, knownFill } from "../capacity";
import { useTheme } from "../theme/ThemeProvider";
import { palette } from "../theme/tokens";
import type { Box } from "../types";

/**
 * How full a container is, drawn so that how much to trust it is visible at a
 * glance:
 *
 * - solid: somebody looked (or LiDAR measured);
 * - hatched: Boxwright's own running estimate;
 * - a dashed outline: nobody has recorded it.
 *
 * An estimate past 100% is drawn full and in the warning colour; the label
 * says by how much.
 */
export function FillBar(props: { box: Box; thin?: boolean; showLabel?: boolean }) {
  const { accent, soft } = useTheme();
  const fill = knownFill(props.box);
  const height = props.thin ? 6 : 8;
  const label = fillLabel(props.box);
  const over = fill !== undefined && fill >= 95;
  const colour = over ? palette.warn : accent;

  const bar =
    fill === undefined ? (
      <View style={[styles.track, styles.unknown, { height, borderRadius: height / 2 }]} />
    ) : (
      <View style={[styles.track, { height, borderRadius: height / 2 }]}>
        {props.box.fillSource === "estimated" ? (
          <View style={{ width: `${clampPct(fill)}%`, height, backgroundColor: soft, overflow: "hidden" }}>
            <Svg width="100%" height={height}>
              <Path d={hatch(height)} stroke={colour} strokeWidth={3} />
            </Svg>
          </View>
        ) : (
          <View style={{ width: `${clampPct(fill)}%`, height, backgroundColor: colour }} />
        )}
      </View>
    );

  return (
    <View style={styles.row} accessible accessibilityLabel={label}>
      <View style={styles.barWrap}>{bar}</View>
      {props.showLabel !== false ? (
        <Text style={[styles.label, over && styles.over]} numberOfLines={1}>
          {label}
        </Text>
      ) : null}
    </View>
  );
}

/** Diagonal strokes every 7 points, wide enough for any phone; the view clips them. */
function hatch(height: number): string {
  const parts: string[] = [];
  for (let x = -height; x < 480; x += 7) parts.push(`M${x} ${height}L${x + height} 0`);
  return parts.join("");
}

function clampPct(pct: number): number {
  return Math.max(0, Math.min(100, pct));
}

const styles = StyleSheet.create({
  row: { flexDirection: "row", alignItems: "center", gap: 10 },
  barWrap: { flex: 1 },
  track: { backgroundColor: palette.hairline, overflow: "hidden" },
  unknown: { backgroundColor: "transparent", borderWidth: 1.5, borderStyle: "dashed", borderColor: palette.unknown },
  label: { fontSize: 13, color: palette.muted, width: 112, textAlign: "right" },
  over: { color: palette.warn, fontWeight: "600" },
});
