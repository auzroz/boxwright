import { useEffect, useRef, useState } from "react";
import { Alert, Linking, Modal, Pressable, StyleSheet, Text, View } from "react-native";
import { useSafeAreaInsets } from "react-native-safe-area-context";
import { DepthCamera, callback } from "boxwright-depth";
import type { DepthCameraMode, DepthCameraRef, DepthCapture, DepthSessionEvent, DepthStatus } from "boxwright-depth";

import { errorMessage } from "../api";
import { depthScreen } from "../depthFallback";
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
  /** Item mode: the way round when depth cannot be had. Fill mode's is Cancel, back to the chips. */
  onSystemCamera?: () => void;
}) {
  const insets = useSafeAreaInsets();
  const camera = useRef<DepthCameraRef | null>(null);
  const [status, setStatus] = useState<DepthStatus | null>(null);
  const [event, setEvent] = useState<DepthSessionEvent | null>(null);
  const [torch, setTorch] = useState(false);
  const [taking, setTaking] = useState(false);
  const [openedAt, setOpenedAt] = useState(0);
  const [now, setNow] = useState(0);
  const poorSince = useRef<number | null>(null);
  const fill = props.mode === "fill";

  // Each opening starts clean, and a clock ticks while it is open: "not
  // started yet" and "struggling for a while" are both questions of time.
  useEffect(() => {
    if (!props.visible) return;
    const t = Date.now();
    setStatus(null);
    setEvent(null);
    setOpenedAt(t);
    setNow(t);
    poorSince.current = null;
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, [props.visible]);

  function onStatus(s: DepthStatus): void {
    if (s.tracking === "normal") poorSince.current = null;
    else poorSince.current ??= Date.now();
    setStatus(s);
  }

  const screen = depthScreen({ mode: props.mode, status, event, openedAt, now, poorSince: poorSince.current });
  const fallbackLabel = fill ? "Choose how full instead" : "Use the system camera";
  const fallback = fill ? props.onCancel : props.onSystemCamera;

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


  return (
    <Modal visible={props.visible} animationType="slide" presentationStyle="fullScreen" onRequestClose={props.onCancel}>
      <View style={styles.root}>
        {props.visible && (
          <DepthCamera
            style={StyleSheet.absoluteFill}
            mode={props.mode}
            active={props.visible}
            torch={torch}
            onStatus={callback(onStatus)}
            onSessionEvent={callback((e: DepthSessionEvent) => setEvent(e))}
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
        {!screen.failed && (
          <View style={[styles.hintWrap, { top: insets.top + 12 }]} pointerEvents="none">
            <Text style={styles.hint} accessibilityLiveRegion="polite">
              {screen.hint}
            </Text>
          </View>
        )}
        {screen.failed && (
          <View style={styles.failure} accessibilityRole="alert">
            <Text style={styles.failureText}>{screen.problem}</Text>
            {screen.openSettings && (
              <Pressable accessibilityRole="button" onPress={() => void Linking.openSettings()} style={styles.failureButton}>
                <Text style={styles.failureButtonText}>Open Settings</Text>
              </Pressable>
            )}
            {fallback && (
              <Pressable accessibilityRole="button" onPress={fallback} style={styles.failureButton}>
                <Text style={styles.failureButtonText}>{fallbackLabel}</Text>
              </Pressable>
            )}
          </View>
        )}
        <View style={[styles.bottom, { paddingBottom: Math.max(insets.bottom, 16) }]}>
          <View style={styles.bar}>
            <Pressable accessibilityRole="button" onPress={props.onCancel} style={styles.side}>
              <Text style={styles.text}>Cancel</Text>
            </Pressable>
            <Pressable
              accessibilityRole="button"
              accessibilityLabel={fill ? "Measure" : "Take photo"}
              accessibilityState={{ disabled: taking || !screen.canShoot }}
              onPress={() => void shoot()}
              disabled={taking || !screen.canShoot}
              style={[styles.shutter, (taking || !screen.canShoot) && styles.disabled]}
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
          {fallback && screen.offerFallback && !screen.failed && (
            <Pressable accessibilityRole="button" onPress={fallback} style={styles.system}>
              <Text style={styles.text}>{fallbackLabel}</Text>
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
  failure: {
    position: "absolute",
    left: 24,
    right: 24,
    top: "30%",
    gap: 12,
    padding: 20,
    borderRadius: 18,
    backgroundColor: "rgba(0,0,0,0.7)",
  },
  failureText: { color: "#fff", fontSize: 17, lineHeight: 23, textAlign: "center" },
  failureButton: {
    minHeight: MIN_TARGET,
    borderRadius: 12,
    backgroundColor: "rgba(255,255,255,0.16)",
    alignItems: "center",
    justifyContent: "center",
  },
  failureButtonText: { color: "#fff", fontSize: 17, fontWeight: "600" },
  shutter: { width: 72, height: 72, borderRadius: 36, backgroundColor: "#fff", borderWidth: 4, borderColor: "#bbb" },
  disabled: { opacity: 0.4 },
});
