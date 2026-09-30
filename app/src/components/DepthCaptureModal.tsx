import { useRef, useState } from "react";
import { Alert, Modal, Pressable, StyleSheet, Text, View } from "react-native";
import { useSafeAreaInsets } from "react-native-safe-area-context";
import { DepthCamera, callback } from "boxwright-depth";
import type { DepthCameraMode, DepthCameraRef, DepthCapture, DepthStatus } from "boxwright-depth";

import { errorMessage } from "../api";
import { FILL_GUIDE } from "../lidar";
import { MIN_TARGET } from "../theme/tokens";

/**
 * Boxwright's own camera: an ARKit preview that keeps the LiDAR depth with the
 * photo, full screen over whatever opened it.
 *
 * Item mode is "Take photo" on a LiDAR phone, with a way back to the system
 * camera for anyone who prefers it. Fill mode is for measuring how full an
 * open container is: it draws the guide the measurement reads (FILL_GUIDE)
 * and shows how far from straight down the phone is, because past 30 degrees
 * the measurement refuses. The session runs only while this is showing.
 */
export function DepthCaptureModal(props: {
  visible: boolean;
  mode: DepthCameraMode;
  onCapture: (shot: DepthCapture) => void;
  onCancel: () => void;
  onSystemCamera?: () => void;
}) {
  const insets = useSafeAreaInsets();
  const camera = useRef<DepthCameraRef | null>(null);
  const [status, setStatus] = useState<DepthStatus | null>(null);
  const [torch, setTorch] = useState(false);
  const [taking, setTaking] = useState(false);
  const fill = props.mode === "fill";

  async function shoot(): Promise<void> {
    if (!camera.current || taking) return;
    setTaking(true);
    try {
      props.onCapture(await camera.current.capture());
    } catch (err) {
      Alert.alert("Could not take the photo", errorMessage(err));
    } finally {
      setTaking(false);
    }
  }

  const tilt = status ? Math.round(status.tiltFromDownDeg) : undefined;
  const hint = !status
    ? "Starting the camera…"
    : status.tracking !== "normal"
      ? "Move the phone slowly so it can find its bearings."
      : fill
        ? tilt !== undefined && tilt > 30
          ? `Tilted ${tilt}°. Hold it flatter, looking straight down.`
          : "Frame the open container inside the box, looking straight down."
        : status.depthOK
          ? "Ready."
          : "Too close or too far to measure. The photo still works.";

  return (
    <Modal visible={props.visible} animationType="slide" presentationStyle="fullScreen" onRequestClose={props.onCancel}>
      <View style={styles.root}>
        {props.visible && (
          <DepthCamera
            style={StyleSheet.absoluteFill}
            mode={props.mode}
            active={props.visible}
            torch={torch}
            onStatus={callback((s: DepthStatus) => setStatus(s))}
            hybridRef={callback((ref: DepthCameraRef) => {
              camera.current = ref;
            })}
          />
        )}
        {fill && (
          <View
            pointerEvents="none"
            style={[
              styles.guide,
              {
                left: `${FILL_GUIDE.x * 100}%`,
                top: `${FILL_GUIDE.y * 100}%`,
                width: `${FILL_GUIDE.w * 100}%`,
                height: `${FILL_GUIDE.h * 100}%`,
              },
            ]}
          />
        )}
        <View style={[styles.hintWrap, { top: insets.top + 12 }]} pointerEvents="none">
          <Text style={styles.hint} accessibilityLiveRegion="polite">
            {hint}
          </Text>
        </View>
        <View style={[styles.bottom, { paddingBottom: Math.max(insets.bottom, 16) }]}>
          <View style={styles.bar}>
            <Pressable accessibilityRole="button" onPress={props.onCancel} style={styles.side}>
              <Text style={styles.text}>Cancel</Text>
            </Pressable>
            <Pressable
              accessibilityRole="button"
              accessibilityLabel={fill ? "Measure" : "Take photo"}
              onPress={() => void shoot()}
              disabled={taking}
              style={[styles.shutter, taking && styles.disabled]}
            />
            <Pressable
              accessibilityRole="button"
              accessibilityLabel="Light"
              accessibilityState={{ selected: torch }}
              onPress={() => setTorch((t) => !t)}
              style={styles.side}
            >
              <Text style={styles.text}>{torch ? "Light on" : "Light"}</Text>
            </Pressable>
          </View>
          {props.onSystemCamera && (
            <Pressable accessibilityRole="button" onPress={props.onSystemCamera} style={styles.system}>
              <Text style={styles.text}>Use the system camera</Text>
            </Pressable>
          )}
        </View>
      </View>
    </Modal>
  );
}

const styles = StyleSheet.create({
  root: { flex: 1, backgroundColor: "#000" },
  guide: { position: "absolute", borderWidth: 2, borderColor: "#fff", borderRadius: 6 },
  hintWrap: { position: "absolute", left: 16, right: 16, alignItems: "center" },
  hint: {
    color: "#fff",
    fontSize: 16,
    textAlign: "center",
    backgroundColor: "rgba(0,0,0,0.45)",
    paddingHorizontal: 14,
    paddingVertical: 8,
    borderRadius: 14,
    overflow: "hidden",
  },
  bottom: { position: "absolute", left: 0, right: 0, bottom: 0, gap: 8, paddingTop: 16, backgroundColor: "rgba(0,0,0,0.35)" },
  bar: { flexDirection: "row", alignItems: "center", justifyContent: "space-around" },
  side: { minWidth: 88, minHeight: MIN_TARGET, alignItems: "center", justifyContent: "center" },
  text: { color: "#fff", fontSize: 17 },
  system: { alignSelf: "center", minHeight: MIN_TARGET, justifyContent: "center", paddingHorizontal: 12 },
  shutter: { width: 72, height: 72, borderRadius: 36, backgroundColor: "#fff", borderWidth: 4, borderColor: "#bbb" },
  disabled: { opacity: 0.4 },
});
