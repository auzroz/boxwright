import { secretShowsInput } from "./secret";

test("an empty token is typed into", () => {
  expect(secretShowsInput(false, "")).toBe(true);
});

// The bug: focus on an empty field, type "d" -- the value is no longer empty,
// and the input vanished mid-word. Focus sets editing, so it stays.
test("a token being typed stays an input after its first character", () => {
  expect(secretShowsInput(true, "d")).toBe(true);
});

test("a saved token not being edited shows dots", () => {
  expect(secretShowsInput(false, "demo")).toBe(false);
});
