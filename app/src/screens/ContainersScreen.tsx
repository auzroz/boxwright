import { useEffect, useState } from "react";
import { ActivityIndicator, Pressable, ScrollView, StyleSheet, Text, View } from "react-native";

import { boxes as fetchBoxList, errorMessage, setContainers } from "../api";
import { plural } from "../draft";
import { palette, radius, space, type } from "../theme/tokens";
import type { Access, Box, ContainerSet, ContainerType } from "../types";
import { FillChoices } from "../components/FillCheck";
import { UnitSwitch } from "../components/UnitSwitch";
import { Button, Checkbox, Chip, ChipRow, Segmented } from "../ui/controls";
import { FillBar } from "../ui/FillBar";
import { Group, Notice, TextField } from "../ui/surfaces";
import { Header, Screen } from "../ui/Screen";
import { formatDimsIn, formatVolume, parseCapacityIn, parseDimsIn, useUnits } from "../units";

const NEW_TYPE = "\u0000new";

const ACCESS = [
  { value: "easy", label: "Easy" },
  { value: "normal", label: "Normal" },
  { value: "deep", label: "Deep" },
] as const;

/**
 * Record what kind of container each chosen location is: its type, how much
 * it holds, its inside size -- and, optionally, how easy it is to reach and
 * how full it is right now.
 *
 * Several at once, because a storage unit is usually many of the same thing:
 * tick every one of a kind, say so once. The type names are the user's own;
 * the ones offered are the ones already on their containers, and nothing is
 * shipped. Everything is optional except a capacity for a new type, and only
 * what is filled in is written -- nothing else about a container is touched.
 */
export function ContainersScreen(props: { onDone: () => void; backLabel?: string }) {
  const system = useUnits();
  const [boxes, setBoxes] = useState<Box[] | null>(null);
  const [types, setTypes] = useState<ContainerType[]>([]);
  const [error, setError] = useState("");
  const [note, setNote] = useState("");
  const [busy, setBusy] = useState(false);
  const [selected, setSelected] = useState<Record<string, boolean>>({});
  /** "" = nothing chosen, NEW_TYPE = a new type, otherwise a type name. */
  const [typeChoice, setTypeChoice] = useState("");
  const [newName, setNewName] = useState("");
  const [capacityText, setCapacityText] = useState("");
  const [interiorText, setInteriorText] = useState("");
  const [access, setAccess] = useState<Access | undefined>(undefined);
  const [fill, setFill] = useState<number | undefined>(undefined);

  const load = async () => {
    setError("");
    try {
      const res = await fetchBoxList();
      setBoxes(res.boxes.filter((b) => !b.isArea));
      setTypes(Array.isArray(res.containerTypes) ? res.containerTypes : []);
    } catch (err) {
      setBoxes([]);
      setError(errorMessage(err));
    }
  };
  useEffect(() => {
    void load();
  }, []);

  const list = boxes ?? [];
  const ids = list.filter((b) => selected[b.id]).map((b) => b.id);
  const isNew = typeChoice === NEW_TYPE;
  const existing = types.find((t) => t.name === typeChoice);
  const metric = system === "metric";

  /** What will be written, or why not. */
  function plan(): { set: ContainerSet } | { problem: string } {
    const set: ContainerSet = {};
    if (isNew) {
      const name = newName.trim();
      if (name === "") return { problem: "Name the new type, as you would call it." };
      const litres = parseCapacityIn(capacityText, system);
      if (litres === undefined) {
        return { problem: `Say how much it holds, like ${metric ? "“100” (litres)" : "“27” (gallons)"}.` };
      }
      set.containerType = name;
      set.capacityL = litres;
      if (interiorText.trim() !== "") {
        const inside = parseDimsIn(interiorText, system);
        if (!inside) return { problem: `Inside size is three numbers, like ${metric ? "66 x 41 x 33" : "26 x 16 x 13"}.` };
        set.interiorCm = inside;
      }
    } else if (existing) {
      set.containerType = existing.name;
      set.capacityL = existing.capacityL;
      if (existing.interiorCm) set.interiorCm = existing.interiorCm;
    }
    if (access !== undefined) set.access = access;
    if (fill !== undefined) set.fill = { pct: fill, source: "observed", at: new Date().toISOString() };
    if (Object.keys(set).length === 0) return { problem: "Choose a type, or what to record." };
    return { set };
  }

  // Nothing to record yet is not an error to shout about; the button says so.
  const ready = plan();
  const nothingChosen = "problem" in ready && typeChoice === "" && access === undefined && fill === undefined;

  async function apply() {
    const p = plan();
    if ("problem" in p) {
      setError(p.problem);
      return;
    }
    setBusy(true);
    setError("");
    setNote("");
    try {
      const res = await setContainers({ ids, set: p.set });
      const failed = res.results.filter((r) => (r.error ?? "") !== "");
      const saved = res.results.length - failed.length;
      if (failed.length > 0) {
        setError(`${failed.length} of ${ids.length} could not be saved: ${failed[0]?.error ?? "unknown error"}`);
      }
      if (saved > 0) setNote(`Recorded on ${saved} ${plural(saved, "container", "containers")}.`);
      setSelected({});
      setFill(undefined);
      await load();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }

  const sheet =
    ids.length === 0 ? undefined : (
      <ScrollView style={styles.sheet} contentContainerStyle={styles.sheetBody} keyboardShouldPersistTaps="handled">
        <View style={styles.sheetHead}>
          <Text style={[type.title, styles.grow]}>
            {ids.length} selected {plural(ids.length, "is a", "are a")}…
          </Text>
          <Button label="Clear" kind="quiet" compact onPress={() => setSelected({})} />
        </View>
        <ChipRow>
          {types.map((t) => (
            <Chip key={t.name} label={t.name} selected={typeChoice === t.name} onPress={() => setTypeChoice(typeChoice === t.name ? "" : t.name)} />
          ))}
          <Chip label="+ New type" dashed={!isNew} selected={isNew} onPress={() => setTypeChoice(isNew ? "" : NEW_TYPE)} />
        </ChipRow>
        {existing && (
          <View style={styles.summary}>
            <View style={styles.grow}>
              <Text style={type.title}>
                {formatVolume(existing.capacityL, system)}
                {existing.interiorCm ? ` · ${formatDimsIn(existing.interiorCm, system)} inside` : ""}
              </Text>
              <Text style={type.caption}>From your other {existing.name} containers</Text>
            </View>
            <UnitSwitch system={system} kind="volume" />
          </View>
        )}
        {isNew && (
          <>
            <TextField label="Type name, as you call it" value={newName} onChange={setNewName} autoCapitalize="sentences" />
            <TextField
              label={`How much it holds, in ${metric ? "litres" : "gallons"}`}
              value={capacityText}
              onChange={setCapacityText}
              keyboardType="decimal-pad"
              placeholder={metric ? "100" : "27"}
              right={<UnitSwitch system={system} kind="volume" />}
            />
            <TextField
              label={`Inside size in ${metric ? "cm" : "inches"} (optional)`}
              value={interiorText}
              onChange={setInteriorText}
              keyboardType="numbers-and-punctuation"
              placeholder={metric ? "66 x 41 x 33" : "26 x 16 x 13"}
            />
          </>
        )}
        <View style={styles.field}>
          <Text style={type.label}>How easy to reach · optional</Text>
          <Segmented<Exclude<Access, "">>
            options={ACCESS}
            value={access === "" ? undefined : access}
            onChange={(a) => setAccess(access === a ? undefined : a)}
            accessibilityLabel="How easy to reach"
          />
        </View>
        <View style={styles.field}>
          <Text style={type.label}>How full right now · optional</Text>
          <FillChoices name={`the ${ids.length} selected`} value={fill} onPick={(pct) => setFill(fill === pct ? undefined : pct)} />
        </View>
        {error !== "" && <Notice tone="warn">{error}</Notice>}
        <Button
          label={nothingChosen ? "Choose what to record" : `Record on ${ids.length} ${plural(ids.length, "container", "containers")}`}
          busy={busy}
          disabled={nothingChosen || (isNew && newName.trim() === "")}
          onPress={() => void apply()}
        />
      </ScrollView>
    );

  return (
    <Screen footer={sheet} sheet>
      <Header
        back={{ label: props.backLabel ?? "Locations", onPress: props.onDone, disabled: busy }}
        title="Container sizes"
        subtitle="Tick the ones that are the same, then say what they are. Boxwright uses the size to tell when something won’t fit, and never guesses one you haven’t given."
      />
      {boxes === null && <ActivityIndicator />}
      {ids.length === 0 && error !== "" && <Notice tone="warn">{error}</Notice>}
      {note !== "" && <Notice tone="ok">{note}</Notice>}
      {boxes !== null && list.length === 0 && error === "" && (
        <Notice>Choose where Boxwright may file things first. Their sizes are recorded here.</Notice>
      )}
      {list.length > 0 && (
        <Group inset={52}>
          {list.map((b) => {
            const on = selected[b.id] === true;
            const kind = [
              b.containerType || "",
              typeof b.capacityL === "number" ? formatVolume(b.capacityL, system) : "",
            ].filter((s) => s !== "");
            return (
              <Pressable
                key={b.id}
                style={({ pressed }) => [styles.row, pressed && styles.pressed]}
                accessibilityRole="checkbox"
                accessibilityState={{ checked: on }}
                accessibilityLabel={b.name}
                onPress={() => setSelected((prev) => ({ ...prev, [b.id]: !on }))}
              >
                <Checkbox checked={on} />
                <View style={styles.rowText}>
                  <View style={styles.rowHead}>
                    <Text style={[styles.name, styles.grow]} numberOfLines={1}>
                      {b.name}
                    </Text>
                    <Text style={type.caption} numberOfLines={1}>
                      {kind.length > 0 ? kind.join(" · ") : "Size not recorded"}
                    </Text>
                  </View>
                  <FillBar box={b} thin />
                </View>
              </Pressable>
            );
          })}
        </Group>
      )}
    </Screen>
  );
}

const styles = StyleSheet.create({
  grow: { flex: 1 },
  row: { minHeight: 64, paddingHorizontal: space.lg, paddingVertical: space.md, flexDirection: "row", alignItems: "center", gap: space.md },
  pressed: { backgroundColor: palette.hairline },
  rowText: { flex: 1, gap: 6 },
  rowHead: { flexDirection: "row", alignItems: "baseline", gap: space.sm },
  name: { fontSize: 16, fontWeight: "600", color: palette.ink },
  sheet: { maxHeight: 380 },
  sheetBody: { gap: 14, paddingBottom: space.sm },
  sheetHead: { flexDirection: "row", alignItems: "center" },
  summary: {
    flexDirection: "row",
    alignItems: "center",
    gap: space.sm,
    borderWidth: 1,
    borderColor: palette.line,
    borderRadius: radius.md,
    padding: space.md,
    backgroundColor: palette.surface,
  },
  field: { gap: space.sm },
});
