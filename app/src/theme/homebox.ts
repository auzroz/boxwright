// The user's Homebox colours, so the two apps they use for one inventory look
// like they belong together.
//
// Homebox's web UI syncs its theme name to the user's settings, and /status
// reports it (homeboxTheme). Only the name travels; the colours are the
// primary of each theme in Homebox v0.26.2's frontend/assets/css/main.css
// (sysadminsmedia/homebox, AGPL-3.0, which is DaisyUI's palette), converted
// from HSL. Only the ACCENT is taken: the paper ground and ink stay, dark
// themes included, so every screen keeps the contrast it was designed with.

import { KRAFT, palette } from "./tokens";
import type { Accent } from "./tokens";

/** Each Homebox theme's primary colour. A name not listed keeps Boxwright's own. */
export const HOMEBOX_PRIMARY: Readonly<Record<string, string>> = {
  homebox: "#5C7F67",
  garden: "#5C7F67",
  aqua: "#09E9F1",
  black: "#343232",
  bumblebee: "#E0A82E",
  cmyk: "#44ADEE",
  corporate: "#4B6BFB",
  cupcake: "#65C3C8",
  cyberpunk: "#FF7598",
  dracula: "#FF7AC6",
  emerald: "#66CC8A",
  fantasy: "#6E0B75",
  forest: "#1EB854",
  halloween: "#F28C18",
  light: "#570DF8",
  lofi: "#0D0D0D",
  luxury: "#FFFFFF",
  pastel: "#D1C1D7",
  retro: "#EF9995",
  synthwave: "#E779C1",
  valentine: "#E96D7B",
  wireframe: "#B8B8B8",
  autumn: "#8C0327",
  business: "#1C4F82",
  acid: "#FF00F2",
  lemonade: "#529B03",
  night: "#3ABFF8",
  coffee: "#DC944C",
  winter: "#057AFF",
};

/** White text on the accent (buttons) must read at least this well: WCAG AA. */
export const MIN_TEXT_CONTRAST = 4.5;
/** The accent as text or an outline on the paper ground: WCAG AA for UI. */
export const MIN_UI_CONTRAST = 3;

/**
 * The accent for a Homebox theme name, or Boxwright's own when there is none
 * or it is not one we know.
 *
 * Many Homebox themes have a light primary (aqua, pastel, a white one), which
 * is fine behind Homebox's dark text and unreadable behind Boxwright's white
 * button labels. So the colour is darkened, keeping its hue, until white text
 * on it passes AA -- the colour is recognisably theirs, and every button stays
 * readable.
 */
export function accentForTheme(theme: string | undefined): Accent {
  const hex = theme ? HOMEBOX_PRIMARY[theme.trim().toLowerCase()] : undefined;
  return hex ? accentFor(hex) : KRAFT;
}

export function accentFor(hex: string): Accent {
  const rgb = parseHex(hex);
  if (!rgb) return KRAFT;
  const white = { r: 255, g: 255, b: 255 };
  const ground = parseHex(palette.ground)!;
  const [h, s, l0] = rgbToHsl(rgb);
  let l = l0;
  let colour = rgb;
  while (l > 0 && (contrast(white, colour) < MIN_TEXT_CONTRAST || contrast(ground, colour) < MIN_UI_CONTRAST)) {
    l = Math.max(0, l - 0.01);
    colour = hslToRgb(h, s, l);
  }
  const accent = toHex(colour);
  return { accent, soft: toHex(mix(colour, white, 0.12)) };
}

type RGB = { r: number; g: number; b: number };

function parseHex(hex: string): RGB | undefined {
  const m = /^#?([0-9a-f]{6})$/i.exec(hex.trim());
  if (!m) return undefined;
  const n = parseInt(m[1]!, 16);
  return { r: (n >> 16) & 255, g: (n >> 8) & 255, b: n & 255 };
}

function toHex(c: RGB): string {
  return `#${[c.r, c.g, c.b].map((v) => Math.round(v).toString(16).padStart(2, "0")).join("").toUpperCase()}`;
}

/** `amount` of `a` over `b`. */
function mix(a: RGB, b: RGB, amount: number): RGB {
  return { r: a.r * amount + b.r * (1 - amount), g: a.g * amount + b.g * (1 - amount), b: a.b * amount + b.b * (1 - amount) };
}

function luminance(c: RGB): number {
  const lin = (v: number) => {
    const x = v / 255;
    return x <= 0.03928 ? x / 12.92 : ((x + 0.055) / 1.055) ** 2.4;
  };
  return 0.2126 * lin(c.r) + 0.7152 * lin(c.g) + 0.0722 * lin(c.b);
}

/** The WCAG contrast ratio between two colours, 1 to 21. */
export function contrast(a: RGB | string, b: RGB | string): number {
  const x = typeof a === "string" ? parseHex(a) : a;
  const y = typeof b === "string" ? parseHex(b) : b;
  if (!x || !y) return 1;
  const [hi, lo] = [luminance(x), luminance(y)].sort((p, q) => q - p) as [number, number];
  return (hi + 0.05) / (lo + 0.05);
}

function rgbToHsl(c: RGB): [number, number, number] {
  const r = c.r / 255;
  const g = c.g / 255;
  const b = c.b / 255;
  const max = Math.max(r, g, b);
  const min = Math.min(r, g, b);
  const l = (max + min) / 2;
  if (max === min) return [0, 0, l];
  const d = max - min;
  const s = l > 0.5 ? d / (2 - max - min) : d / (max + min);
  const h = max === r ? (g - b) / d + (g < b ? 6 : 0) : max === g ? (b - r) / d + 2 : (r - g) / d + 4;
  return [h / 6, s, l];
}

function hslToRgb(h: number, s: number, l: number): RGB {
  if (s === 0) return { r: l * 255, g: l * 255, b: l * 255 };
  const q = l < 0.5 ? l * (1 + s) : l + s - l * s;
  const p = 2 * l - q;
  const hue = (t: number) => {
    let x = t;
    if (x < 0) x += 1;
    if (x > 1) x -= 1;
    if (x < 1 / 6) return p + (q - p) * 6 * x;
    if (x < 1 / 2) return q;
    if (x < 2 / 3) return p + (q - p) * (2 / 3 - x) * 6;
    return p;
  };
  return { r: hue(h + 1 / 3) * 255, g: hue(h) * 255, b: hue(h - 1 / 3) * 255 };
}
