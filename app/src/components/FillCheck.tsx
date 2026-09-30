import { useState } from "react";
import { Pressable, StyleSheet, Text, View } from "react-native";
import { DepthKit } from "boxwright-depth";
import type { DepthCapture } from "boxwright-depth";

import { setContainers } from "../api";
import { knownFill, normalizeDims } from "../capacity";
import { measureFillFromFile } from "../lidar";
import { setQueuedFill } from "../offline";
import { discardTempFiles } from "../photo";
import { useTheme } from "../theme/ThemeProvider";
import { palette, radius, space, type } from "../theme/tokens";
import type { Box, FillObservation, QueuedEntry } from "../types";
import { Button } from "../ui/controls";
import { Card, Notice } from "../ui/surfaces";
import { DepthCaptureModal } from "./DepthCaptureModal";

export const FILL_CHOICES: readonly { label: string; pct: number }[] = [
  { label: "Empty", pct: 0 },
  { label: "¼", pct: 25 },
  { label: "½", pct: 50 },
  { label: "¾", pct: 75 },
  { label: "Full", pct: 100 },
];

/**
 * "How full is it now?" for each container something was just put into.
 *
 * Asked here, after filing, rather than before: a photo of one item files the
 * moment its destination is tapped, and the answer is most accurate once the
 * item is actually in. It is optional -- nothing here blocks anything -- and
 * an answer is an OBSERVATION the server keeps over its own estimates. While
 * the capture is still waiting on this phone the answer rides with it;
 * otherwise it is saved straight away.
 */
export function FillCheck(props: { boxes: Box[]; captureId: string; queued: QueuedEntry[] }) {
  const [status, setStatus] = useState<Record<string, string>>({});
  /** Containers whose fill is dealt with: saved, or riding with the queue. */
  const [settled, setSettled] = useState<Record<string, boolean>>({});
  const [picked, setPicked] = useState<Record<string, number>>({});
  /** The container being measured with the depth camera, if any. */
  const [measuring, setMeasuring] = useState<Box | null>(null);
  if (props.boxes.length === 0) return null;

  /**
   * A depth photo of the open container from above, read against its inside
   * size. The photo and its depth are thrown away as soon as the number is
   * out: only the percentage goes anywhere.
   */
  async function measured(box: Box, shot: DepthCapture): Promise<void> {
    setMeasuring(null);
    const inside = normalizeDims(box.interiorCm);
    if (!shot.depthPath || !inside) {
      discardTempFiles(shot.photoPath, shot.depthPath);
      setStatus((s) => ({ ...s, [box.id]: "No depth in that photo. Choose how full it is instead." }));
      return;
    }
    const res = await measureFillFromFile(shot.depthPath, inside);
    discardTempFiles(shot.photoPath, shot.depthPath);
    if ("refused" in res) {
      setStatus((s) => ({ ...s, [box.id]: `${res.refused} Or choose how full it is.` }));
      return;
    }
    await record(box, res.fillPct, "lidar");
  }

  async function record(box: Box, pct: number, source: FillObservation["source"] = "observed"): Promise<void> {
    setPicked((p) => ({ ...p, [box.id]: pct }));
    const fill: FillObservation = { pct, source, at: new Date().toISOString() };
    const waiting = props.queued.some((e) => e.boxId === box.id);
    if (waiting && setQueuedFill(props.captureId, box.id, fill)) {
      setStatus((s) => ({ ...s, [box.id]: "Noted. It goes with the items when they file." }));
      setSettled((s) => ({ ...s, [box.id]: true }));
      return;
    }
    setStatus((s) => ({ ...s, [box.id]: source === "lidar" ? `Measured about ${Math.round(pct)}% full. Saving…` : "Saving…" }));
    try {
      const res = await setContainers({ ids: [box.id], set: { fill } });
      const error = res.results[0]?.error;
      if (error) throw new Error(error);
      setStatus((s) => ({ ...s, [box.id]: source === "lidar" ? `Measured about ${Math.round(pct)}% full. Saved.` : "Saved." }));
      setSettled((s) => ({ ...s, [box.id]: true }));
    } catch {
      setStatus((s) => ({ ...s, [box.id]: "Not saved. You will be asked again next time." }));
    }
  }

  return (
    <View style={styles.list}>
      {props.boxes.map((box) => {
        const guess = knownFill(box);
        // Measuring needs the inside size -- depth to the contents means
        // nothing without depth to the bottom -- and a phone with LiDAR.
        const canMeasure = DepthKit.isSupported && normalizeDims(box.interiorCm) !== undefined;
        return (
          <Card key={box.id}>
            <View style={styles.head}>
              <Text style={type.title}>How full is {box.name} now?</Text>
              <Text style={type.label}>
                {settled[box.id]
                  ? `Recorded as ${FILL_CHOICES.find((c) => c.pct === picked[box.id])?.label.toLowerCase() ?? `${Math.round(picked[box.id] ?? 0)}%`}.`
                  : guess === undefined
                  ? "Not recorded yet. Optional, and it helps the next suggestion."
                  : `Boxwright’s guess is about ${Math.round(guess)}%. Your answer replaces it.`}
              </Text>
            </View>
            {settled[box.id] ? (
              <Notice tone="ok">{status[box.id] ?? "Saved."}</Notice>
            ) : (
              <>
                <FillChoices
                  name={box.name}
                  value={picked[box.id]}
                  onPick={(pct) => void record(box, pct)}
                />
                {status[box.id] ? <Text style={type.caption}>{status[box.id]}</Text> : null}
                {canMeasure && (
                  <Button
                    label="Measure it with the camera"
                    kind="secondary"
                    compact
                    icon="ruler"
                    onPress={() => setMeasuring(box)}
                  />
                )}
              </>
            )}
          </Card>
        );
      })}
      <DepthCaptureModal
        visible={measuring !== null}
        mode="fill"
        onCapture={(shot) => {
          if (measuring) void measured(measuring, shot);
        }}
        onCancel={() => setMeasuring(null)}
      />
    </View>
  );
}

/** Five buttons, each with a little container drawn filled to its level. */
export function FillChoices(props: { name: string; value: number | undefined; onPick: (pct: number) => void }) {
  const { accent, soft } = useTheme();
  return (
    <View style={styles.grid} accessibilityRole="radiogroup" accessibilityLabel={`How full ${props.name} is`}>
      {FILL_CHOICES.map(({ label, pct }) => {
        const on = props.value === pct;
        const ink = on ? accent : palette.faint;
        return (
          <Pressable
            key={label}
            accessibilityRole="radio"
            accessibilityState={{ checked: on }}
            accessibilityLabel={label === "Empty" || label === "Full" ? label : `${pct}% full`}
            onPress={() => props.onPick(pct)}
            style={[styles.choice, on && { borderColor: accent, borderWidth: 2, backgroundColor: soft }]}
          >
            <View style={[styles.glyph, { borderColor: ink }]}>
              <View style={[styles.glyphFill, { height: `${pct}%`, backgroundColor: ink }]} />
            </View>
            <Text style={[styles.choiceText, on && styles.choiceTextOn]}>{label}</Text>
          </Pressable>
        );
      })}
    </View>
  );
}

const styles = StyleSheet.create({
  list: { gap: space.md },
  head: { gap: 3 },
  grid: { flexDirection: "row", gap: 6 },
  choice: {
    flex: 1,
    minHeight: 56,
    borderWidth: 1,
    borderColor: palette.line,
    borderRadius: radius.md,
    backgroundColor: palette.surface,
    alignItems: "center",
    justifyContent: "center",
    gap: 6,
  },
  glyph: { width: 22, height: 14, borderWidth: 1.5, borderRadius: 3, justifyContent: "flex-end", overflow: "hidden" },
  glyphFill: { width: "100%" },
  choiceText: { fontSize: 13, color: palette.ink },
  choiceTextOn: { fontWeight: "600" },
});
