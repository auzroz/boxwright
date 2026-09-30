import { useState } from "react";
import { Alert, Pressable, StyleSheet, Text, View } from "react-native";

import { errorMessage, probe } from "../api";
import type { ProbeResult } from "../api";
import { sameDestination, transportWarnings, useConnection, validateConnection } from "../connection";
import type { Connection } from "../connection";
import { ConnectionChangeBlocked, switchConnection, useQueue } from "../offline";
import { useTheme } from "../theme/ThemeProvider";
import { MIN_TARGET, palette, radius, space, type } from "../theme/tokens";
import { setPrefs, usePrefs } from "../prefs";
import { Button, Segmented, ToggleRow } from "../ui/controls";
import { setUnits, useUnits } from "../units";
import type { UnitSystem } from "../units";
import { Icon } from "../ui/Icon";
import { Group, Notice, TextField } from "../ui/surfaces";
import { Header, Screen } from "../ui/Screen";

/**
 * Where this phone sends things: the Boxwright server, its token, and --
 * only when that server allows it -- a Homebox of the user's own.
 *
 * Shown first on a fresh install, because every other screen depends on it,
 * and reachable from the home screen afterwards. Saving does not require a
 * successful test: someone may well be setting this up with no signal, and the
 * offline queue exists for exactly that. The test is there so that they CAN
 * find out, before walking into a storage unit, rather than after.
 */
export function ConnectionScreen(props: {
  /** True on a fresh install: there is nowhere to go back to. */
  firstRun: boolean;
  /**
   * Inside first-run setup: numbered, with a way back to the welcome screen,
   * and Continue offered only once a test has passed -- every step after this
   * one reads from the server.
   */
  setup?: { onBack: () => void; step: { index: number; count: number } };
  /** In the demo there is no server to set: only this phone's preferences, and the way out. */
  demo?: { onLeave: () => void; onBack: () => void };
  /** Offered at the foot of Settings. */
  onTryDemo?: () => void;
  /** `moved` is true when the save pointed the app at a different inventory. */
  onDone: (moved: boolean) => void;
}) {
  if (props.demo) return <DemoSettings onLeave={props.demo.onLeave} onBack={props.demo.onBack} />;
  return <ServerSettings {...props} />;
}

function DemoSettings(props: { onLeave: () => void; onBack: () => void }) {
  return (
    <Screen>
      <Header back={{ label: "Back", onPress: props.onBack }} title="Settings" />
      <ThisPhone />
      <Notice
        title="You’re in the demo"
        action={<Button label="Leave the demo" kind="secondary" compact onPress={props.onLeave} />}
      >
        Its inventory is a sample on this phone. Leave it to connect your own server; nothing from the demo comes with you.
      </Notice>
    </Screen>
  );
}

function ServerSettings(props: Parameters<typeof ConnectionScreen>[0]) {
  const { connection } = useConnection();
  const queue = useQueue();
  const [draft, setDraft] = useState<Connection>(connection);
  const [ownHomebox, setOwnHomebox] = useState(connection.homeboxUrl !== "" || connection.homeboxToken !== "");
  const [errors, setErrors] = useState<Partial<Record<keyof Connection, string>>>({});
  const [result, setResult] = useState<ProbeResult | null>(null);
  const [busy, setBusy] = useState<"" | "test" | "save">("");
  function leave(moved: boolean): void {
    props.onDone(moved);
  }

  /** The draft as it would be saved: the Homebox pair only while that section is open. */
  function effective(): Connection {
    return ownHomebox ? draft : { ...draft, homeboxUrl: "", homeboxToken: "" };
  }

  function edit(patch: Partial<Connection>): void {
    setDraft((current) => ({ ...current, ...patch }));
    // A result describes the settings it was run against, not these.
    setResult(null);
    setErrors({});
  }

  function checked(): Connection | null {
    const validated = validateConnection(effective());
    if (!validated.ok) {
      setErrors(validated.errors);
      return null;
    }
    setErrors({});
    return validated.connection;
  }

  async function test() {
    const next = checked();
    if (!next) return;
    setBusy("test");
    setResult(null);
    try {
      setResult(await probe(next));
    } catch (err) {
      setResult({ ok: false, message: errorMessage(err) });
    } finally {
      setBusy("");
    }
  }

  async function save() {
    const next = checked();
    if (!next) return;
    setBusy("save");
    try {
      const moved = !sameDestination(connection, next);
      await switchConnection(next);
      leave(moved);
    } catch (err) {
      if (err instanceof ConnectionChangeBlocked) {
        Alert.alert("Captures are still waiting", err.message);
      } else {
        Alert.alert("Could not save these settings", errorMessage(err));
      }
    } finally {
      setBusy("");
    }
  }

  const validated = validateConnection(effective());
  const warnings = validated.ok ? transportWarnings(validated.connection) : [];

  return (
    <Screen
      footer={
        props.setup && !result?.ok ? (
          <Button label="Test connection" busy={busy === "test"} disabled={busy !== ""} onPress={() => void test()} />
        ) : (
          <>
            <Button label="Test connection" kind="secondary" busy={busy === "test"} disabled={busy !== ""} onPress={() => void test()} />
            <Button
              label={props.setup ? "Continue" : props.firstRun ? "Save and continue" : "Save"}
              busy={busy === "save"}
              disabled={busy !== ""}
              onPress={() => void save()}
            />
          </>
        )
      }
    >
      <Header
        back={
          props.setup
            ? { label: "Welcome", onPress: props.setup.onBack, disabled: busy !== "" }
            : props.firstRun
              ? null
              : { label: "Cancel", onPress: () => leave(false), disabled: busy !== "" }
        }
        step={props.setup?.step}
        title={props.setup ? "Connect your server" : props.firstRun ? "Connect to your server" : "Settings"}
        subtitle={
          props.setup
            ? "The address and access token your Boxwright server was set up with (BOXWRIGHT_API_TOKEN)."
            : "Boxwright runs beside your Homebox, on your own hardware. Enter the address and access token it was set up with (BOXWRIGHT_API_TOKEN)."
        }
      />

      {!props.firstRun && !props.setup && <ThisPhone />}
      {!props.firstRun && !props.setup && <Text style={type.section}>SERVER</Text>}
      <TextField
        label="Server address"
        value={draft.apiUrl}
        placeholder="https://boxwright.example.com"
        error={errors.apiUrl}
        keyboardType="url"
        textContentType="URL"
        literal
        onChange={(apiUrl) => edit({ apiUrl })}
      />
      <SecretField
        label="Access token"
        value={draft.apiToken}
        placeholder="Leave empty only for a server on this device"
        error={errors.apiToken}
        onChange={(apiToken) => edit({ apiToken })}
      />
      {queue.entries.length > 0 && (
        <Text style={type.caption}>
          {queue.entries.length} {queue.entries.length === 1 ? "capture is" : "captures are"} waiting for the current
          server. You can change its token now, but not point Boxwright somewhere else until they have uploaded or been
          discarded.
        </Text>
      )}

      <Group>
        <ToggleRow
          label="Use a Homebox of my own"
          detail="Only for a server set up to accept one (ALLOW_CLIENT_HOMEBOX)."
          value={ownHomebox}
          onChange={(on) => {
            setOwnHomebox(on);
            setResult(null);
            setErrors({});
          }}
        />
      </Group>
      {ownHomebox && (
        <>
          <Text style={type.caption}>Leave the address empty to use the server’s own Homebox under a different account.</Text>
          <TextField
            label="Homebox address"
            value={draft.homeboxUrl}
            placeholder="https://homebox.example.com"
            error={errors.homeboxUrl}
            keyboardType="url"
            textContentType="URL"
            literal
            onChange={(homeboxUrl) => edit({ homeboxUrl })}
          />
          <SecretField
            label="Homebox API key"
            value={draft.homeboxToken}
            placeholder="hb_…"
            error={errors.homeboxToken}
            onChange={(homeboxToken) => edit({ homeboxToken })}
          />
        </>
      )}

      {warnings.map((w) => (
        <Notice key={w} tone="caution">
          {w}
        </Notice>
      ))}

      {result && !result.ok && (
        <Notice tone="warn" title="Could not connect">
          {result.message}
        </Notice>
      )}
      {result && result.ok && (
        <Notice tone="ok" title={`Connected to Boxwright ${result.status.version}`}>
          <View style={styles.checks}>
            <Check ok={result.status.homebox.ok} text={result.status.homebox.ok ? "Homebox is reachable" : "Homebox is not reachable"} />
            <Check
              ok={result.status.identification}
              neutral={!result.status.identification}
              text={
                result.status.identification
                  ? `Identification is on (${result.status.aiProvider})`
                  : "Identification is off. You’ll enter items by hand."
              }
            />
            {result.warnings.map((w) => (
              <Text key={w} style={type.caption}>
                {w}
              </Text>
            ))}
          </View>
        </Notice>
      )}
      {props.onTryDemo && !props.setup && (
        <Button label="Try the demo" kind="quiet" onPress={props.onTryDemo} />
      )}
    </Screen>
  );
}

/**
 * A token: dots unless it is being edited, and never a secure text field.
 *
 * iOS offers to "Save Password?" for any secure field that leaves the screen
 * with text in it, as a sheet over whatever comes next. textContentType "none"
 * and "oneTimeCode" did not stop it, and nor did unmasking the field just
 * before leaving (all tried on the iOS 27 simulator). Without a secure field
 * there is nothing to offer, so the token is shown as dots by this screen
 * rather than by the keyboard, and in plain text only while it is typed.
 */
function SecretField(props: { label: string; value: string; placeholder: string; error?: string; onChange: (v: string) => void }) {
  const { accent } = useTheme();
  const [editing, setEditing] = useState(false);
  if (editing || props.value === "") {
    return (
      <TextField
        label={props.label}
        value={props.value}
        placeholder={props.placeholder}
        error={props.error}
        literal
        textContentType="none"
        autoFocus={editing}
        onEndEditing={() => setEditing(false)}
        onChange={props.onChange}
      />
    );
  }
  return (
    <View style={styles.secret}>
      <Text style={type.label}>{props.label}</Text>
      <Pressable
        accessibilityRole="button"
        accessibilityLabel={`${props.label}, set. Change`}
        onPress={() => setEditing(true)}
        style={[styles.secretBox, props.error !== undefined && styles.secretError]}
      >
        <Text style={styles.dots} numberOfLines={1}>
          {"•".repeat(Math.min(props.value.length, 24))}
        </Text>
        <Text style={[styles.change, { color: accent }]}>Change</Text>
      </Pressable>
      {props.error !== undefined ? <Text style={[type.caption, styles.errorText]}>{props.error}</Text> : null}
    </View>
  );
}

/**
 * Preferences that belong to this phone rather than the server: they change
 * how things look and read here, and are never sent anywhere.
 */
function ThisPhone() {
  const p = usePrefs();
  const system = useUnits();
  return (
    <>
      <Text style={type.section}>THIS PHONE</Text>
      <Group>
        <View style={styles.pref}>
          <Text style={type.body}>Measurements</Text>
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
              ? `Buttons and highlights use your Homebox theme (${p.homeboxTheme}).`
              : "Uses your Homebox theme’s colour once the server reports it."
          }
          value={p.matchHomebox !== false}
          onChange={(on) => setPrefs({ matchHomebox: on })}
        />
      </Group>
    </>
  );
}

function Check(props: { ok: boolean; neutral?: boolean; text: string }) {
  const colour = props.ok ? palette.ok : props.neutral ? palette.muted : palette.warn;
  return (
    <View style={styles.check}>
      <Icon name={props.ok ? "check" : props.neutral ? "alert" : "close"} size={18} color={colour} strokeWidth={2.2} />
      <Text style={[styles.checkText, { color: colour }]}>{props.text}</Text>
    </View>
  );
}

const styles = StyleSheet.create({
  secret: { gap: 6 },
  secretBox: {
    minHeight: MIN_TARGET,
    borderWidth: 1,
    borderColor: palette.line,
    borderRadius: radius.md,
    paddingHorizontal: space.md,
    backgroundColor: palette.surface,
    flexDirection: "row",
    alignItems: "center",
    gap: space.sm,
  },
  secretError: { borderColor: palette.warn },
  dots: { flex: 1, fontSize: 17, letterSpacing: 2, color: palette.ink },
  change: { fontSize: 15, fontWeight: "600" },
  errorText: { color: palette.warn },
  pref: { padding: space.md, gap: space.sm },
  checks: { gap: space.xs },
  check: { flexDirection: "row", alignItems: "center", gap: space.sm },
  checkText: { fontSize: 15, flex: 1 },
});
