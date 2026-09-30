import { useEffect, useState } from "react";
import { DepthKit } from "boxwright-depth";

import { TextField } from "../ui/surfaces";
import { dimsFieldText, parseDimsIn, useUnits } from "../units";
import type { Dims, ItemDraft } from "../types";
import { UnitSwitch } from "./UnitSwitch";

/**
 * The item's size, as text a person can correct.
 *
 * Kept as text while it is being typed and read only when they finish: "30 x"
 * is on its way to being a size, not a wrong one. What they type is a
 * MEASUREMENT (source "manual") -- the engine will then rule out a container
 * it does not fit -- so the label says where the current figure came from,
 * and an estimate is marked as one. Clearing the field goes back to the size
 * bucket.
 */
export function SizeField(props: {
  dims: Dims | null;
  source: ItemDraft["dimensionsSource"];
  onChange: (dims: Dims | null) => void;
}) {
  const system = useUnits();
  const shown = props.dims ? dimsFieldText(props.dims, system) : "";
  const [text, setText] = useState(shown);
  const [bad, setBad] = useState(false);
  // A new estimate, a LiDAR measurement or a switch of units replaces what
  // was shown.
  useEffect(() => {
    setText(shown);
    setBad(false);
  }, [shown]);

  const origin =
    props.source === "lidar"
      ? "Size, measured with LiDAR"
      : props.source === "manual"
        ? "Size, as you entered it"
        : props.source === "vision"
          ? "Size, estimated from the photo"
          : "Size (optional)";
  const example = system === "metric" ? "30 x 20 x 10" : "12 x 8 x 4";

  function commit(): void {
    // Only an edit is a measurement. Tapping in and out of the field must not
    // turn the model's estimate into a "manual" size -- which the engine
    // treats as a fact that can rule containers out -- and in inches the
    // shown text is rounded, so reading it back would move the size too.
    if (text === shown) {
      setBad(false);
      return;
    }
    if (text.trim() === "") {
      setBad(false);
      if (props.dims) props.onChange(null);
      return;
    }
    const parsed = parseDimsIn(text, system);
    if (!parsed) {
      setBad(true);
      return;
    }
    setBad(false);
    props.onChange(parsed);
  }

  return (
    <TextField
      label={origin}
      value={text}
      onChange={setText}
      onEndEditing={commit}
      placeholder={example}
      keyboardType="numbers-and-punctuation"
      returnKeyType="done"
      right={<UnitSwitch system={system} kind="length" />}
      accessibilityLabel={`Size in ${system === "metric" ? "centimetres" : "inches"}, three numbers`}
      hint={
        props.source !== "vision"
          ? undefined
          : DepthKit.isSupported
            ? "Correct it if you know; a measured size can rule out containers it will not fit."
            : "Estimated from what it is. This iPhone can’t measure with the camera, so type the size if you know it: a typed size lets Boxwright rule out containers it won’t fit."
      }
      error={bad ? `Three numbers, like ${example}. Add cm or in to use the other unit.` : undefined}
    />
  );
}
