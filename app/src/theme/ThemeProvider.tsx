import { createContext, useContext } from "react";
import type { ReactNode } from "react";

import { KRAFT } from "./tokens";
import type { Accent } from "./tokens";

const ThemeContext = createContext<Accent>(KRAFT);

/**
 * The accent for everything below it. Boxwright's own amber unless something
 * above says otherwise -- which is how the user's Homebox colours get in.
 */
export function ThemeProvider(props: { accent?: Accent; children: ReactNode }) {
  return <ThemeContext.Provider value={props.accent ?? KRAFT}>{props.children}</ThemeContext.Provider>;
}

export function useTheme(): Accent {
  return useContext(ThemeContext);
}
