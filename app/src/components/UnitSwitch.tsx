import { Segmented } from "../ui/controls";
import { setUnits } from "../units";
import type { UnitSystem } from "../units";

/**
 * The one place units are switched: a small "cm | in" (or "L | gal") beside a
 * measurement, where the question comes up. It flips the whole app, not just
 * this field -- a phone is metric or imperial, not both -- and only how
 * numbers are shown and read; everything stored stays metric.
 */
export function UnitSwitch(props: { system: UnitSystem; kind: "length" | "volume" }) {
  const options =
    props.kind === "length"
      ? ([
          { value: "metric", label: "cm" },
          { value: "imperial", label: "in" },
        ] as const)
      : ([
          { value: "metric", label: "L" },
          { value: "imperial", label: "gal" },
        ] as const);
  return (
    <Segmented<UnitSystem>
      small
      options={options}
      value={props.system}
      onChange={setUnits}
      accessibilityLabel="Units"
    />
  );
}
