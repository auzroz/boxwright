/**
 * Whether a token field shows its input (plain text) or its dots.
 *
 * The input while it is being edited, and while it is empty -- there is
 * nothing to hide and nowhere else to type. Being edited is set by focus, not
 * by the value: deciding from the value alone swapped the input out for the
 * dots the moment the first character arrived.
 */
export function secretShowsInput(editing: boolean, value: string): boolean {
  return editing || value === "";
}
