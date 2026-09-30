import { useEffect, useState } from "react";
import { ActivityIndicator, Pressable, StyleSheet, Text, View } from "react-native";

import { adoptLocations, errorMessage, locations as fetchLocations, setLocations } from "../api";
import { plural } from "../draft";
import { useTheme } from "../theme/ThemeProvider";
import { palette, space, type } from "../theme/tokens";
import type { LocationNode } from "../types";
import { Button, Checkbox } from "../ui/controls";
import { Group, Notice } from "../ui/surfaces";
import { Header, Screen } from "../ui/Screen";
import { ContainersScreen } from "./ContainersScreen";

/**
 * Choose which locations Boxwright may file into.
 *
 * Every location in the user's Homebox is listed, indented by depth, with
 * nothing filtered out. Boxwright used to work this out for itself -- a
 * location was a container if it had no locations under it, was not an
 * unannotated top-level entry, and so on -- and every one of those rules was
 * generalised from a single inventory. Against a flat Homebox of twenty empty
 * boxes they all said no, so the engine reported that nothing fitted and
 * suggested buying another container, forever.
 *
 * Whether somewhere is a place you put things is not a fact about its shape.
 * It is a decision, and it is the user's.
 */
export function LocationsScreen(props: {
  onDone: () => void;
  /**
   * Inside first-run setup: numbered, back goes to the server step, and at
   * least one place is required -- with none, every item would come back
   * "no existing container fits". Sizes are the next step, not a button here.
   */
  setup?: { onBack: () => void; step: { index: number; count: number } };
}) {
  const { accent } = useTheme();
  const [sizing, setSizing] = useState(false);
  const [rows, setRows] = useState<LocationNode[] | null>(null);
  const [error, setError] = useState("");
  /** Only what the user CHANGED. Each one is a write, so the untouched ones are not sent. */
  const [changed, setChanged] = useState<Record<string, boolean>>({});
  const [busy, setBusy] = useState(false);
  const [note, setNote] = useState("");

  const load = async () => {
    setError("");
    try {
      const res = await fetchLocations();
      setRows(res.locations);
      setChanged({});
    } catch (err) {
      setRows([]);
      setError(errorMessage(err));
    }
  };

  useEffect(() => {
    void load();
  }, []);

  if (sizing) return <ContainersScreen onDone={() => setSizing(false)} />;

  const isOn = (row: LocationNode) => changed[row.id] ?? row.eligible;
  const list = rows ?? [];
  const chosen = list.filter(isOn).length;
  const pending = Object.keys(changed).length;

  async function save() {
    const body = Object.entries(changed).map(([id, eligible]) => ({ id, eligible }));
    if (body.length === 0) {
      props.onDone();
      return;
    }
    setBusy(true);
    setNote("");
    try {
      const res = await setLocations(body);
      const failed = res.results.filter((r) => (r.error ?? "") !== "");
      if (failed.length > 0) {
        // Partial success is normal here and must not be reported as either a
        // clean save or a clean failure: the ones that landed have landed.
        setError(`${failed.length} of ${body.length} could not be saved: ${failed[0]?.error ?? "unknown error"}`);
        await load();
        return;
      }
      props.onDone();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }

  async function adopt() {
    setBusy(true);
    setError("");
    setNote("");
    try {
      const res = await adoptLocations();
      setNote(
        res.marked === 0
          ? "Nothing to bring over. No locations had container settings on them already."
          : `Brought over ${res.marked} ${plural(res.marked, "location", "locations")} that already had container settings.`,
      );
      await load();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }

  function toggle(row: LocationNode) {
    const on = isOn(row);
    setChanged((prev) => {
      const next = { ...prev };
      // Ticking something back to where it started is not a change, and
      // sending it would be a pointless write to Homebox.
      if (!on === row.eligible) delete next[row.id];
      else next[row.id] = !on;
      return next;
    });
  }

  return (
    <Screen
      footer={
        <>
          {list.length > 0 && (
            <Text style={type.callout}>
              {chosen} of {list.length} selected
              {pending > 0 ? ` · ${pending} unsaved ${plural(pending, "change", "changes")}` : ""}
            </Text>
          )}
          <Button
            label={props.setup ? (chosen === 0 ? "Choose at least one" : "Continue") : pending > 0 ? "Save selection" : "Done"}
            busy={busy}
            disabled={props.setup !== undefined && chosen === 0}
            onPress={() => void save()}
          />
        </>
      }
    >
      <Header
        back={props.setup ? { label: "Server", onPress: props.setup.onBack, disabled: busy } : { label: "Back", onPress: props.onDone, disabled: busy }}
        step={props.setup?.step}
        title="Where may it file things?"
        subtitle="Tick anywhere you’d actually put something. Boxwright only ever suggests these, and leaves the rest of your Homebox alone."
      />

      {rows === null && <ActivityIndicator />}
      {error !== "" && (
        <Notice tone="warn" action={<Button label="Try again" kind="secondary" compact onPress={() => void load()} />}>
          {error}
        </Notice>
      )}
      {note !== "" && <Notice tone="ok">{note}</Notice>}

      {rows !== null && list.length === 0 && error === "" && (
        <Notice title="No locations yet">
          There are no locations in your Homebox yet. Create one there first (a room, a shelf, a box, whatever fits how you already organise) and come back.
        </Notice>
      )}

      {list.length > 0 && (
        <Group inset={52}>
          {list.map((row) => {
            const on = isOn(row);
            return (
              <Pressable
                key={row.id}
                style={({ pressed }) => [styles.row, { paddingLeft: space.lg + Math.min(row.depth, 4) * 20 }, pressed && styles.pressed]}
                accessibilityRole="checkbox"
                accessibilityState={{ checked: on }}
                accessibilityLabel={`${row.name}${row.parentName ? `, in ${row.parentName}` : ""}`}
                onPress={() => toggle(row)}
              >
                <Checkbox checked={on} />
                <View style={styles.rowText}>
                  <Text style={styles.name}>{row.name}</Text>
                  <Text style={type.caption}>
                    {[
                      row.parentName ? `in ${row.parentName}` : "top level",
                      row.hasChildren ? "holds other locations" : "",
                      row.itemCount > 0 ? `${row.itemCount} ${plural(row.itemCount, "item", "items")}` : "",
                    ]
                      .filter((s) => s !== "")
                      .join(" · ")}
                  </Text>
                </View>
              </Pressable>
            );
          })}
        </Group>
      )}

      {/*
        Sizes are recorded on the containers already chosen, so this comes
        after the selection -- and unsaved ticks must be saved first, or the
        container someone just ticked would be missing from the list.
      */}
      {!props.setup && (
        <Button
          label={pending > 0 ? "Save your selection to record sizes" : "Record container sizes"}
          kind="secondary"
          icon="ruler"
          disabled={busy || pending > 0}
          onPress={() => setSizing(true)}
        />
      )}
      {/* For anyone upgrading, and for anyone who filled the capacity and
          access fields in by hand: it never overrides a choice already made. */}
      <Pressable style={styles.link} accessibilityRole="button" disabled={busy} onPress={() => void adopt()}>
        <Text style={[styles.linkText, { color: accent }, busy && styles.disabled]}>Bring over locations I already set up</Text>
      </Pressable>
    </Screen>
  );
}

const styles = StyleSheet.create({
  row: { minHeight: 58, paddingRight: space.lg, paddingVertical: 10, flexDirection: "row", alignItems: "center", gap: space.md },
  pressed: { backgroundColor: palette.hairline },
  rowText: { flex: 1, gap: 2 },
  name: { fontSize: 16, fontWeight: "600", color: palette.ink },
  link: { minHeight: 44, justifyContent: "center", alignItems: "center" },
  linkText: { fontSize: 15, fontWeight: "600" },
  disabled: { opacity: 0.4 },
});
