import { useState } from "react";
import { ActivityIndicator, Alert, Pressable, ScrollView, StyleSheet, Text, TextInput, View } from "react-native";

import { errorMessage, probe } from "./api";
import type { ProbeResult } from "./api";
import { sameDestination, transportWarnings, useConnection, validateConnection } from "./connection";
import type { Connection } from "./connection";
import { ConnectionChangeBlocked, switchConnection, useQueue } from "./offline";

/**
 * Where this phone sends things: the Boxwright server, its token, and --
 * only when that server allows it -- a Homebox of the user's own.
 *
 * Shown first on a fresh install, because every other screen depends on it,
 * and reachable from the capture screen afterwards. Saving does not require a
 * successful test: someone may well be setting this up with no signal, and the
 * offline queue exists for exactly that. The test is there so that they CAN
 * find out, before walking into a storage unit, rather than after.
 */
export function ConnectionScreen(props: {
  /** True on a fresh install: there is nowhere to go back to. */
  firstRun: boolean;
  /** `moved` is true when the save pointed the app at a different inventory. */
  onDone: (moved: boolean) => void;
}) {
  const { connection } = useConnection();
  const queue = useQueue();
  const [draft, setDraft] = useState<Connection>(connection);
  const [ownHomebox, setOwnHomebox] = useState(connection.homeboxUrl !== "" || connection.homeboxToken !== "");
  const [errors, setErrors] = useState<Partial<Record<keyof Connection, string>>>({});
  const [result, setResult] = useState<ProbeResult | null>(null);
  const [busy, setBusy] = useState(false);

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
    setBusy(true);
    setResult(null);
    try {
      setResult(await probe(next));
    } catch (err) {
      setResult({ ok: false, message: errorMessage(err) });
    } finally {
      setBusy(false);
    }
  }

  async function save() {
    const next = checked();
    if (!next) return;
    setBusy(true);
    try {
      const moved = !sameDestination(connection, next);
      await switchConnection(next);
      props.onDone(moved);
    } catch (err) {
      if (err instanceof ConnectionChangeBlocked) {
        Alert.alert("Captures are still waiting", err.message);
      } else {
        Alert.alert("Could not save these settings", errorMessage(err));
      }
    } finally {
      setBusy(false);
    }
  }

  const validated = validateConnection(effective());
  const warnings = validated.ok ? transportWarnings(validated.connection) : [];

  return (
    // automaticallyAdjustKeyboardInsets: the form fits one page, so without an
    // inset for the keyboard there is nothing to scroll, and the keyboard sits
    // over Save -- found tapping through this screen on an iOS 27 simulator.
    <ScrollView contentContainerStyle={styles.form} automaticallyAdjustKeyboardInsets keyboardShouldPersistTaps="handled">
      <Text style={styles.lede}>{props.firstRun ? "Connect to your Boxwright server" : "Server settings"}</Text>
      <Text style={styles.hint}>
        Boxwright runs beside your Homebox, on your own hardware. Enter the address and access token
        it was set up with (BOXWRIGHT_API_TOKEN).
      </Text>

      <LabelledInput
        label="Server address"
        value={draft.apiUrl}
        placeholder="https://boxwright.example.com"
        error={errors.apiUrl}
        keyboardType="url"
        onChange={(apiUrl) => edit({ apiUrl })}
      />
      <LabelledInput
        label="Access token"
        value={draft.apiToken}
        placeholder="Leave empty only for a server on this device"
        error={errors.apiToken}
        secret
        onChange={(apiToken) => edit({ apiToken })}
      />

      <Pressable
        style={styles.toggle}
        accessibilityRole="switch"
        accessibilityState={{ checked: ownHomebox }}
        onPress={() => {
          setOwnHomebox(!ownHomebox);
          setResult(null);
          setErrors({});
        }}
      >
        <Text style={styles.toggleText}>{ownHomebox ? "☑" : "☐"} Use a Homebox of my own</Text>
      </Pressable>
      {ownHomebox && (
        <>
          <Text style={styles.hint}>
            Only for a server set up to accept one (ALLOW_CLIENT_HOMEBOX). Leave the address empty to
            use the server's own Homebox under a different account.
          </Text>
          <LabelledInput
            label="Homebox address"
            value={draft.homeboxUrl}
            placeholder="https://homebox.example.com"
            error={errors.homeboxUrl}
            keyboardType="url"
            onChange={(homeboxUrl) => edit({ homeboxUrl })}
          />
          <LabelledInput
            label="Homebox API key"
            value={draft.homeboxToken}
            placeholder="hb_…"
            error={errors.homeboxToken}
            secret
            onChange={(homeboxToken) => edit({ homeboxToken })}
          />
        </>
      )}

      {warnings.map((w) => (
        <Text key={w} style={styles.warning}>
          {w}
        </Text>
      ))}

      {result && !result.ok && (
        <View style={styles.failureBlock}>
          <Text style={styles.failureRow}>{result.message}</Text>
        </View>
      )}
      {result && result.ok && (
        <View style={styles.okBlock}>
          <Text style={styles.okHead}>Connected to Boxwright {result.status.version}</Text>
          <Text style={styles.okRow}>
            {result.status.homebox.ok ? "✓ Homebox is reachable" : "✗ Homebox is not reachable"}
          </Text>
          <Text style={styles.okRow}>
            {result.status.identification
              ? `✓ Identification is on (${result.status.aiProvider})`
              : "– Identification is off; items are entered by hand"}
          </Text>
          {result.warnings.map((w) => (
            <Text key={w} style={styles.hint}>
              {w}
            </Text>
          ))}
        </View>
      )}

      {queue.entries.length > 0 && (
        <Text style={styles.hint}>
          {queue.entries.length} {queue.entries.length === 1 ? "capture is" : "captures are"} waiting for
          the current server. You can change its token now, but not point Boxwright somewhere else until
          they have uploaded or been discarded.
        </Text>
      )}

      <Pressable style={[styles.secondary, busy && styles.disabled]} disabled={busy} onPress={() => void test()}>
        <Text style={styles.secondaryText}>Test connection</Text>
      </Pressable>
      <Pressable style={[styles.primary, busy && styles.disabled]} disabled={busy} onPress={() => void save()}>
        <Text style={styles.primaryText}>{props.firstRun ? "Save and continue" : "Save"}</Text>
      </Pressable>
      {!props.firstRun && (
        <Pressable disabled={busy} onPress={() => props.onDone(false)}>
          <Text style={styles.link}>Cancel</Text>
        </Pressable>
      )}
      {busy && <ActivityIndicator />}
    </ScrollView>
  );
}

function LabelledInput(props: {
  label: string;
  value: string;
  placeholder: string;
  error: string | undefined;
  secret?: boolean;
  keyboardType?: "url";
  onChange: (value: string) => void;
}) {
  return (
    <View style={styles.field}>
      <Text style={styles.fieldLabel}>{props.label}</Text>
      <TextInput
        style={[styles.input, props.error !== undefined && styles.inputError]}
        value={props.value}
        placeholder={props.placeholder}
        onChangeText={props.onChange}
        autoCapitalize="none"
        autoCorrect={false}
        spellCheck={false}
        keyboardType={props.keyboardType ?? "default"}
        secureTextEntry={props.secret}
        // Keeps iOS from offering to save a server token as a website password.
        textContentType={props.secret ? "none" : "URL"}
        accessibilityLabel={props.label}
      />
      {props.error !== undefined && <Text style={styles.errorText}>{props.error}</Text>}
    </View>
  );
}

// Kept in step with the palette in App.tsx by hand; there are too few shared
// tokens yet to be worth a theme module.
const styles = StyleSheet.create({
  form: { gap: 12, paddingBottom: 48 },
  lede: { fontSize: 17, lineHeight: 24 },
  hint: { color: "#666", fontSize: 14 },
  warning: { color: "#5c4a12", fontSize: 14 },
  field: { gap: 4 },
  fieldLabel: { fontSize: 14, color: "#333" },
  input: { borderWidth: 1, borderColor: "#ccc", borderRadius: 8, padding: 10, fontSize: 16 },
  inputError: { borderColor: "#b54747" },
  errorText: { fontSize: 13, color: "#7a2f2f" },
  toggle: { borderWidth: 1, borderColor: "#ccc", borderRadius: 8, padding: 10, minHeight: 44, justifyContent: "center" },
  toggleText: { fontSize: 16 },
  primary: { backgroundColor: "#1a1a1a", borderRadius: 8, padding: 14, alignItems: "center" },
  primaryText: { color: "#fff", fontSize: 16, fontWeight: "600" },
  secondary: { borderWidth: 1, borderColor: "#1a1a1a", borderRadius: 8, padding: 14, alignItems: "center" },
  secondaryText: { color: "#1a1a1a", fontSize: 16, fontWeight: "600" },
  disabled: { opacity: 0.4 },
  link: { fontSize: 15, color: "#5c4a12", textDecorationLine: "underline", textAlign: "center" },
  failureBlock: {
    borderWidth: 1, borderColor: "#e0a3a3", backgroundColor: "#fdf0f0", borderRadius: 8, padding: 12, gap: 8,
  },
  failureRow: { fontSize: 14, color: "#7a2f2f" },
  okBlock: {
    borderWidth: 1, borderColor: "#a3c9a8", backgroundColor: "#f0f8f1", borderRadius: 8, padding: 12, gap: 6,
  },
  okHead: { fontSize: 15, fontWeight: "600", color: "#1f4d27" },
  okRow: { fontSize: 14, color: "#1f4d27" },
});
