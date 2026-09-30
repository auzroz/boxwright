import Svg, { Circle, Path, Rect } from "react-native-svg";

import { palette } from "../theme/tokens";

export type IconName =
  | "back"
  | "forward"
  | "check"
  | "camera"
  | "photo"
  | "paste"
  | "box"
  | "settings"
  | "trash"
  | "ruler"
  | "alert"
  | "close"
  | "plus";

/**
 * The handful of line icons the screens use, drawn at 24x24 and scaled.
 * Decorative: whatever an icon sits in carries the accessible label.
 */
export function Icon(props: { name: IconName; size?: number; color?: string; strokeWidth?: number }) {
  const size = props.size ?? 22;
  const color = props.color ?? palette.ink;
  return (
    <Svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke={color}
      strokeWidth={props.strokeWidth ?? 1.8}
      strokeLinecap="round"
      strokeLinejoin="round"
      accessibilityElementsHidden
      importantForAccessibility="no-hide-descendants"
    >
      {shape(props.name)}
    </Svg>
  );
}

function shape(name: IconName) {
  switch (name) {
    case "back":
      return <Path d="M15 6l-6 6 6 6" />;
    case "forward":
      return <Path d="M9 6l6 6-6 6" />;
    case "check":
      return <Path d="M5 12.5l4.5 4.5L19 7" />;
    case "camera":
      return (
        <>
          <Path d="M4 8h3l2-3h6l2 3h3v11H4z" />
          <Circle cx={12} cy={13} r={3.5} />
        </>
      );
    case "photo":
      return (
        <>
          <Rect x={3} y={5} width={18} height={14} rx={2} />
          <Circle cx={8.5} cy={10} r={1.5} />
          <Path d="M21 16l-5-5-9 8" />
        </>
      );
    case "paste":
      return (
        <>
          <Rect x={6} y={4} width={12} height={17} rx={2} />
          <Path d="M9 4h6v3H9z" />
        </>
      );
    case "box":
      return (
        <>
          <Path d="M3 8l9-4 9 4v9l-9 4-9-4z" />
          <Path d="M3 8l9 4 9-4M12 12v9" />
        </>
      );
    case "settings":
      return (
        <>
          <Path d="M4 7h10M18 7h2M4 17h4M12 17h8" />
          <Circle cx={16} cy={7} r={2} />
          <Circle cx={10} cy={17} r={2} />
        </>
      );
    case "trash":
      return <Path d="M5 7h14M10 7V5h4v2M7 7l1 13h8l1-13" />;
    case "ruler":
      return <Path d="M4 15l11-11 5 5-11 11zM8 11l2 2M11 8l2 2" />;
    case "alert":
      return (
        <>
          <Circle cx={12} cy={12} r={9} />
          <Path d="M12 7.5v5.5M12 16.5v.01" />
        </>
      );
    case "close":
      return <Path d="M6 6l12 12M18 6L6 18" />;
    case "plus":
      return <Path d="M12 5v14M5 12h14" />;
  }
}
