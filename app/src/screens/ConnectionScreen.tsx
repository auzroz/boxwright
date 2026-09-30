import { useState } from "react";
import { Alert, StyleSheet, Text, View } from "react-native";

import { errorMessage, probe } from "../api";
import type { ProbeResult } from "../api";
import { sameDestination, transportWarnings, useConnection, validateConnection } from "../connection";
import type { Connection } from "../connection";
import { ConnectionChangeBlocked, switchConnection, useQueue } from "../offline";
import { palette, space, type } from "../theme/tokens";
import { Button, ToggleRow } from "../ui/controls";
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
  /** `moved` is true when the save pointed the app at a different inventory. */
  onDone: (moved: boolean) => void;
}) {
  const { connection } = useConnection();
  const queue = useQueue();
  const [draft, setDraft] = useState<Connection>(connection);
  const [ownHomebox, setOwnHomebox] = useState(connection.homeboxUrl !== "" || connection.homeboxToken !== "");
  const [errors, setErrors] = useState<Partial<Record<keyof Connection, string>>>({});
  const [result, setResult] = useState<ProbeResult | null>(null);
  const [busy, setBusy] = useState<"" | "test" | "save">("");

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
      props.onDone(moved);
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
        <>
          <Button label="Test connection" kind="secondary" busy={busy === "test"} disabled={busy !== ""} onPress={() => void test()} />
          <Button label={props.firstRun ? "Save and continue" : "Save"} busy={busy === "save"} disabled={busy !== ""} onPress={() => void save()} />
        </>
      }
    >
      <Header
        back={props.firstRun ? null : { label: "Cancel", onPress: () => props.onDone(false), disabled: busy !== "" }}
        title={props.firstRun ? "Connect to your server" : "Server settings"}
        subtitle="Boxwright runs beside your Homebox, on your own hardware. Enter the address and access token it was set up with (BOXWRIGHT_API_TOKEN)."
      />

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
      <TextField
        label="Access token"
        value={draft.apiToken}
        placeholder="Leave empty only for a server on this device"
        error={errors.apiToken}
        secret
        literal
        // Keeps iOS from offering to save a server token as a website password.
        textContentType="none"
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
          <TextField
            label="Homebox API key"
            value={draft.homeboxToken}
            placeholder="hb_…"
            error={errors.homeboxToken}
            secret
            literal
            textContentType="none"
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
    </Screen>
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
  checks: { gap: space.xs },
  check: { flexDirection: "row", alignItems: "center", gap: space.sm },
  checkText: { fontSize: 15, flex: 1 },
});
