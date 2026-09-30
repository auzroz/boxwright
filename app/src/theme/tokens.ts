// Direction A ("Kraft"): a warm paper ground, dark ink, one accent colour.
//
// Every colour on screen comes from here or from the accent in ThemeProvider,
// so the screens never carry a palette of their own. Light only: the app is
// pinned to light appearance (Info.plist UIUserInterfaceStyle) until there is
// a dark palette to switch to.

/** The neutrals. The accent is separate because the user's Homebox can set it. */
export const palette = {
  ground: "#F7F3EC",
  surface: "#FFFFFF",
  ink: "#1E1B16",
  muted: "#5E574C",
  faint: "#9A9082",
  line: "#E3DCCF",
  hairline: "#EFE9DF",
  placeholder: "#E6DED1",
  /** An unchecked checkbox or radio: visible without competing with the ink. */
  control: "#B8AE9F",
  /** The dashed outline of something nobody has recorded. */
  unknown: "#C9BFAF",
  warn: "#8A3B2E",
  warnSoft: "#F6E1DC",
  caution: "#7A4F0C",
  cautionSoft: "#FBEFD9",
  ok: "#2F5D3A",
  okSoft: "#E4EFE3",
  onAccent: "#FFFFFF",
} as const;

export interface Accent {
  /** Buttons, links, selection. White text on it reads at 4.5:1 or better. */
  accent: string;
  /** A tint of the accent for selected rows and badges. */
  soft: string;
}

/** Boxwright's own accent: kraft-paper amber. */
export const KRAFT: Accent = { accent: "#A8521E", soft: "#F4E4D4" };

export const radius = { sm: 8, md: 12, lg: 16, xl: 18, xxl: 22 } as const;

export const space = { xs: 4, sm: 8, md: 12, lg: 16, xl: 20, xxl: 24 } as const;

/** The PostScript name of the bundled display face (assets/fonts, SIL OFL). */
export const DISPLAY_FONT = "Fraunces72pt-SemiBold";

export const type = {
  display: { fontFamily: DISPLAY_FONT, fontSize: 30, lineHeight: 35, letterSpacing: -0.3, color: palette.ink },
  brand: { fontFamily: DISPLAY_FONT, fontSize: 28, lineHeight: 33, letterSpacing: -0.3, color: palette.ink },
  heading: { fontFamily: DISPLAY_FONT, fontSize: 22, lineHeight: 27, color: palette.ink },
  title: { fontSize: 17, lineHeight: 22, fontWeight: "600", color: palette.ink },
  body: { fontSize: 17, lineHeight: 23, color: palette.ink },
  callout: { fontSize: 15, lineHeight: 21, color: palette.muted },
  label: { fontSize: 14, lineHeight: 19, color: palette.muted },
  caption: { fontSize: 13, lineHeight: 17, color: palette.muted },
  section: { fontSize: 12, lineHeight: 16, fontWeight: "700", letterSpacing: 0.8, color: palette.muted },
} as const;

/** The smallest thing a thumb should have to hit. */
export const MIN_TARGET = 44;
