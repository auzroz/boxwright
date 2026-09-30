// First-run setup: welcome, connect, where things may go, container sizes,
// preferences, ready. The middle four reuse the screens that do the same job
// afterwards, in a numbered variant, so there is one of each form to keep
// right. What decides where setup starts and resumes is src/setup.ts.

import { useEffect, useState } from "react";
import { StyleSheet, Text, View } from "react-native";
import { DepthKit } from "boxwright-depth";

import { useConnection } from "../connection";
import { plural } from "../draft";
import { cachedBoxes, refreshBoxCache } from "../offline";
import { setPrefs, usePrefs } from "../prefs";
import { nextStep, previousStep, stepLabel } from "../setup";
import type { SetupStep } from "../setup";
import { useTheme } from "../theme/ThemeProvider";
import { palette, radius, space, type } from "../theme/tokens";
import { Button, Segmented, ToggleRow } from "../ui/controls";
import { Icon } from "../ui/Icon";
import type { IconName } from "../ui/Icon";
import { Group, ListRow, Notice } from "../ui/surfaces";
import { Header, Screen } from "../ui/Screen";
import { setUnits, useUnits } from "../units";
import type { UnitSystem } from "../units";
import { ConnectionScreen } from "./ConnectionScreen";
import { ContainersScreen } from "./ContainersScreen";
import { LocationsScreen } from "./LocationsScreen";

export function SetupFlow(props: {
  step: SetupStep;
  onStep: (step: SetupStep) => void;
  /** `moved` as for the connection screen: the app now points somewhere new. */
  onConnected: (moved: boolean) => void;
  /** Setup is over. `photo` when the user chose to take their first one now. */
  onFinish: (photo: boolean) => void;
  /** Offered on the welcome screen when the app has a demo to show. */
  onDemo?: () => void;
}) {
  const step = props.step;
  const label = stepLabel(step);
  const back = () => {
    const prev = previousStep(step);
    if (prev) props.onStep(prev);
  };
  const next = () => {
    const n = nextStep(step);
    if (n === "done") props.onFinish(false);
    else props.onStep(n);
  };

  switch (step) {
    case "welcome":
      return <WelcomeScreen onConnect={next} onDemo={props.onDemo} />;
    case "connect":
      return (
        <ConnectionScreen
          firstRun
          setup={{ onBack: back, step: label! }}
          onDone={(moved) => {
            props.onConnected(moved);
            next();
          }}
        />
      );
    case "locations":
      return <LocationsScreen setup={{ onBack: back, step: label! }} onDone={next} />;
    case "sizes":
      return <ContainersScreen setup={{ onBack: back, step: label! }} onDone={next} />;
    case "prefs":
      return <PrefsScreen step={label!} onBack={back} onDone={next} />;
    case "ready":
      return <ReadyScreen onPhoto={() => props.onFinish(true)} onLookAround={() => props.onFinish(false)} />;
  }
}

function WelcomeScreen(props: { onConnect: () => void; onDemo?: () => void }) {
  const { accent } = useTheme();
  return (
    <Screen
      footer={
        <>
          <Button label="Connect my server" onPress={props.onConnect} />
          {props.onDemo && <Button label="Try the demo" kind="secondary" onPress={props.onDemo} />}
          {props.onDemo && (
            <Text style={[type.caption, styles.center]}>The demo uses a sample inventory on this phone and never goes online.</Text>
          )}
        </>
      }
    >
      <View style={styles.hero}>
        <View style={[styles.mark, { backgroundColor: accent }]}>
          <Icon name="box" size={34} color={palette.onAccent} />
        </View>
        <Text style={[type.display, styles.brand]} accessibilityRole="header">
          Boxwright
        </Text>
        <Text style={type.callout}>
          Photograph what you’re putting away. Boxwright works out what it is, finds the box it belongs in, and files it in
          your Homebox.
        </Text>
      </View>
      <View style={styles.points}>
        <Point icon="camera" title="One photo, many things" body="A whole shelf is split into items for you to check." />
        <Point icon="box" title="The right container" body="Like with like, with room to spare, or a new one when nothing fits." />
        <Point icon="check" title="Yours, on your hardware" body="It talks only to your own server. No account, no tracking." />
      </View>
    </Screen>
  );
}

function Point(props: { icon: IconName; title: string; body: string }) {
  const { accent, soft } = useTheme();
  return (
    <View style={styles.point}>
      <View style={[styles.pointIcon, { backgroundColor: soft }]}>
        <Icon name={props.icon} size={20} color={accent} />
      </View>
      <View style={styles.grow}>
        <Text style={type.title}>{props.title}</Text>
        <Text style={type.label}>{props.body}</Text>
      </View>
    </View>
  );
}

function PrefsScreen(props: { step: { index: number; count: number }; onBack: () => void; onDone: () => void }) {
  const p = usePrefs();
  const system = useUnits();
  return (
    <Screen footer={<Button label="Finish setup" onPress={props.onDone} />}>
      <Header
        back={{ label: "Containers", onPress: props.onBack }}
        step={props.step}
        title="A few preferences"
        subtitle="All of these can be changed later in Settings."
      />
      <Group>
        <View style={styles.pref}>
          <Text style={type.title}>Measurements</Text>
          <Text style={type.label}>How sizes and capacities read. The cm | in switch beside any size changes it too.</Text>
          <Segmented<UnitSystem>
            options={[
              { value: "metric", label: "Metric" },
              { value: "imperial", label: "Imperial" },
            ]}
            value={system}
            onChange={setUnits}
            accessibilityLabel="Measurements"
          />
        </View>
        <ToggleRow
          label="Match my Homebox colours"
          detail={
            p.homeboxTheme
              ? `Your Homebox uses the “${p.homeboxTheme}” theme. Its colour goes on Boxwright’s paper.`
              : "Your server didn’t say which theme your Homebox uses, so Boxwright keeps its own for now."
          }
          value={p.matchHomebox !== false}
          onChange={(on) => setPrefs({ matchHomebox: on })}
        />
      </Group>
      {DepthKit.isSupported ? (
        <Notice tone="ok" title="This iPhone can measure">
          Its LiDAR camera measures each item, and how full an open container is. Camera access is asked for when you
          first take a photo.
        </Notice>
      ) : (
        <Notice title="Sizes come from the photo">
          This iPhone has no LiDAR, so each item’s size is estimated from what it is. Type a size you know and Boxwright
          can rule out containers it won’t fit.
        </Notice>
      )}
    </Screen>
  );
}

function ReadyScreen(props: { onPhoto: () => void; onLookAround: () => void }) {
  const { accent, soft } = useTheme();
  const p = usePrefs();
  const system = useUnits();
  const { connection } = useConnection();
  const [boxes, setBoxes] = useState(() => cachedBoxes());
  useEffect(() => {
    // The places were just chosen; the saved list may predate them.
    void refreshBoxCache().then(() => setBoxes(cachedBoxes()));
  }, []);
  const places = boxes.length;
  const sized = boxes.filter((b) => typeof b.capacityL === "number" && b.capacityL > 0).length;
  const host = hostOnly(connection.apiUrl);
  return (
    <Screen
      footer={
        <>
          <Button label="Take your first photo" icon="camera" onPress={props.onPhoto} />
          <Button label="Look around first" kind="quiet" onPress={props.onLookAround} />
        </>
      }
    >
      <View style={styles.hero}>
        <View style={[styles.mark, { backgroundColor: soft }]}>
          <Icon name="check" size={32} color={accent} strokeWidth={2.4} />
        </View>
        <Text style={type.display} accessibilityRole="header">
          You’re set
        </Text>
        <Text style={type.callout}>
          Boxwright will only ever file into the {places} {plural(places, "place", "places")} you chose.
        </Text>
      </View>
      <Group>
        <ListRow title="Server" right={<Text style={type.label}>{host}</Text>} />
        <ListRow title="Places to file into" right={<Text style={type.label}>{places}</Text>} />
        <ListRow title="Containers with sizes" right={<Text style={type.label}>{sized}</Text>} />
        <ListRow title="Units" right={<Text style={type.label}>{system === "metric" ? "Metric" : "Imperial"}</Text>} />
        <ListRow
          title="Colours"
          right={<Text style={type.label}>{p.matchHomebox !== false && p.homeboxTheme ? "From Homebox" : "Boxwright’s own"}</Text>}
        />
      </Group>
    </Screen>
  );
}

function hostOnly(url: string): string {
  return url.replace(/^https?:\/\//i, "").replace(/\/.*$/, "");
}

const styles = StyleSheet.create({
  grow: { flex: 1, gap: 2 },
  center: { textAlign: "center" },
  hero: { gap: space.md, marginTop: space.xxl },
  mark: { width: 64, height: 64, borderRadius: radius.lg, alignItems: "center", justifyContent: "center" },
  brand: { fontSize: 38, lineHeight: 44 },
  points: { gap: space.lg, marginTop: space.sm },
  point: { flexDirection: "row", gap: 14, alignItems: "flex-start" },
  pointIcon: { width: 40, height: 40, borderRadius: 12, alignItems: "center", justifyContent: "center" },
  pref: { padding: space.md, gap: space.sm },
});
