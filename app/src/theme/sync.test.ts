import { resetStore } from "../../test/react-native-keychain";
import { resetKv } from "../../test/react-native-mmkv";

import { EMPTY_CONNECTION, saveConnection } from "../connection";
import { prefs, resetPrefsForTest, setPrefs } from "../prefs";
import { accentForTheme } from "./homebox";
import { accentFrom, forgetHomeboxTheme, refreshHomeboxTheme } from "./sync";
import { KRAFT } from "./tokens";

const realFetch = globalThis.fetch;

beforeEach(async () => {
  resetKv();
  resetStore();
  resetPrefsForTest();
  await saveConnection({ ...EMPTY_CONNECTION, apiUrl: "https://box.example.com", apiToken: "t" });
});

afterEach(() => {
  globalThis.fetch = realFetch;
});

function answer(body: unknown, code = 200) {
  globalThis.fetch = jest.fn(async () => new Response(JSON.stringify(body), { status: code })) as unknown as typeof fetch;
}

const base = { version: "1", apiVersion: 1, identification: true, aiProvider: "none", clientHomebox: false, homebox: { ok: true } };

test("the server's answer is remembered, and survives a restart", async () => {
  answer({ ...base, homeboxTheme: "forest" });
  await refreshHomeboxTheme();
  expect(prefs().homeboxTheme).toBe("forest");
  resetPrefsForTest();
  expect(prefs().homeboxTheme).toBe("forest");
});

test("no signal keeps the last answer", async () => {
  setPrefs({ homeboxTheme: "forest" });
  globalThis.fetch = jest.fn(async () => {
    throw new TypeError("Network request failed");
  }) as unknown as typeof fetch;
  await refreshHomeboxTheme();
  expect(prefs().homeboxTheme).toBe("forest");
});

test("a server that does not know clears it", async () => {
  setPrefs({ homeboxTheme: "forest" });
  answer(base);
  await refreshHomeboxTheme();
  expect(prefs().homeboxTheme).toBe("");
});

test("saving the theme keeps the other preferences", async () => {
  setPrefs({ units: "imperial", matchHomebox: false });
  answer({ ...base, homeboxTheme: "forest" });
  await refreshHomeboxTheme();
  resetPrefsForTest();
  expect(prefs()).toMatchObject({ units: "imperial", matchHomebox: false, homeboxTheme: "forest" });
});

test("moving to another Homebox forgets the old colours", () => {
  setPrefs({ homeboxTheme: "forest" });
  forgetHomeboxTheme();
  expect(prefs().homeboxTheme).toBe("");
});

test("matching is on unless turned off", () => {
  expect(accentFrom({ homeboxTheme: "forest" })).toEqual(accentForTheme("forest"));
  expect(accentFrom({ homeboxTheme: "forest", matchHomebox: false })).toEqual(KRAFT);
  expect(accentFrom({})).toEqual(KRAFT);
});
