import { useState } from "react";
import { StyleSheet, Text, View } from "react-native";

import { categoryKeyFor, pickerOptions, resolveTypedCategory } from "../categories";
import type { Proposed } from "../draft";
import { Button, Chip, ChipRow } from "../ui/controls";
import { Notice, TextField } from "../ui/surfaces";
import { space, type } from "../theme/tokens";
import type { CategoryOption } from "../types";

/**
 * Category as chips over the user's OWN vocabulary, plus a way in for one more.
 *
 * Chips, not a modal or a wheel: this is used standing in a storage unit with a
 * box under one arm, so every option is one thumb-sized tap away with nothing to
 * open or dismiss. The list is served most-used-first, which puts the handful of
 * categories somebody actually files into within reach of a thumb.
 *
 * Open, not closed. The vocabulary is the user's Homebox tags and the model is
 * allowed to propose a category none of them cover, so a picker that could only
 * choose would silently drop that proposal on the floor. "New" is therefore
 * always available -- last in the row, and never the default, because
 * inventing a near-duplicate of an existing tag is the one way this control can
 * do damage.
 */
export function CategoryPicker(props: {
  options: CategoryOption[];
  live: boolean;
  value: string;
  proposed: Proposed | null;
  onChange: (key: string, label: string) => void;
}) {
  const [adding, setAdding] = useState(false);
  const [typed, setTyped] = useState("");

  const shown = pickerOptions(props.options, props.value, props.proposed?.label ?? "");
  // Judged against the live list, not against `proposed`, so a category that
  // arrives in a refresh quietly stops being announced as new.
  // "New" means "filing this will create a Homebox tag", which is true both
  // for a category nobody has offered AND for one that exists in the list but
  // has nothing filed under it yet -- the server reports the latter as
  // inUse:false. Treating only the first as new made an unused seed look like
  // an established category, so a user picking it had no idea a tag was about
  // to appear in their Homebox.
  const isNew = (key: string) => {
    const option = props.options.find((o) => o.key === key);
    return !option || option.inUse === false;
  };

  function commit() {
    const label = typed.trim();
    if (label === "") {
      setAdding(false);
      return;
    }
    // Typing a category that already exists selects it instead of proposing a
    // second one: "Tools" and "tools" are one tag in Homebox, and two chips for
    // it would split the affinity the engine reads off that tag. Matching has
    // to consider LABELS as well as keys -- the chips show "Books & Media"
    // while the wire carries "books-media", so typing exactly what is on
    // screen would otherwise invent a new category.
    const existing = resolveTypedCategory(label, props.options);
    props.onChange(existing?.key ?? categoryKeyFor(label), existing?.label ?? label);
    setTyped("");
    setAdding(false);
  }

  return (
    <View style={styles.field}>
      <Text style={type.label}>Category</Text>
      {!props.live && <Text style={type.caption}>Showing the starter list. Your Homebox categories could not be loaded.</Text>}
      {props.proposed !== null && isNew(props.value) && (
        <Notice tone="caution">
          {props.proposed.byModel
            ? `Suggested “${props.proposed.label}”, which is not one of your categories yet. Filing this creates it. Keep it, or tap another.`
            : `“${props.proposed.label}” will be created as a new category when you file this.`}
        </Notice>
      )}
      <ChipRow>
        {shown.map((option) => {
          const selected = option.key === props.value;
          const fresh = isNew(option.key);
          return (
            <Chip
              key={option.key}
              label={fresh ? `${option.label} · new` : option.label}
              accessibilityLabel={fresh ? `${option.label}, new category` : option.label}
              selected={selected}
              fresh={fresh}
              onPress={() => props.onChange(option.key, option.label)}
            />
          );
        })}
        {!adding && (
          <Chip label="+ New" dashed accessibilityLabel="Add a category that is not listed" onPress={() => setAdding(true)} />
        )}
      </ChipRow>
      {adding && (
        <View style={styles.addRow}>
          <View style={styles.addInput}>
            <TextField
              label="New category"
              value={typed}
              onChange={setTyped}
              placeholder="e.g. Appliances"
              autoFocus
              autoCapitalize="words"
              returnKeyType="done"
              onSubmit={commit}
            />
          </View>
          <Button label="Use" compact disabled={typed.trim() === ""} onPress={commit} />
          <Button
            label="Cancel"
            kind="quiet"
            compact
            onPress={() => {
              setTyped("");
              setAdding(false);
            }}
          />
        </View>
      )}
    </View>
  );
}

const styles = StyleSheet.create({
  field: { gap: space.sm },
  addRow: { flexDirection: "row", alignItems: "flex-end", gap: space.sm },
  addInput: { flex: 1 },
});
