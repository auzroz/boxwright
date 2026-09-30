/**
 * The connection is what every request is sent with, and two of its four
 * values are credentials. What is pinned here: an address someone types
 * becomes the base the backend expects, the client-credential rules match the
 * backend's, and iOS's refusal of plain http to a real hostname is said on
 * the settings screen rather than discovered as "could not reach the backend".
 */
import { resetStore, failNext, store } from "../test/react-native-keychain";

import {
  EMPTY_CONNECTION,
  currentConnection,
  hostOf,
  isLocalHost,
  loadConnection,
  normalizeServerUrl,
  sameDestination,
  saveConnection,
  transportWarnings,
  validateConnection,
} from "./connection";
import type { Connection } from "./connection";

const SERVICE = "app.boxwright.connection.v1";

function conn(patch: Partial<Connection>): Connection {
  return { ...EMPTY_CONNECTION, ...patch };
}

describe("normalizeServerUrl", () => {
  test.each([
    ["https://box.example.com", "https://box.example.com"],
    ["  https://box.example.com/  ", "https://box.example.com"],
    ["HTTP://192.168.1.20:8080", "http://192.168.1.20:8080"],
    ["https://example.com/boxwright/", "https://example.com/boxwright"],
    ["http://[fd00::1]:8080", "http://[fd00::1]:8080"],
  ])("boxwright %s -> %s", (raw, want) => {
    expect(normalizeServerUrl(raw)).toEqual({ ok: true, value: want });
  });

  test.each([
    ["", "Enter the server address."],
    ["192.168.1.20:8080", "Start the address with http://"],
    ["box.example.com", "Start the address with http://"],
    ["ftp://box.example.com", "Use an http:// or https:// address."],
    ["https://box.example.com/api", "without /api"],
    ["https://box.example.com/api/v1/", "without /api"],
    ["https://box.example.com?x=1", "Enter just the server address"],
    ["https://box example.com", "Enter just the server address"],
  ])("boxwright %j is refused", (raw, message) => {
    const got = normalizeServerUrl(raw);
    expect(got.ok).toBe(false);
    if (!got.ok) expect(got.error).toContain(message);
  });

  // HOMEBOX_BASE_URL includes /api, and the backend feeds the header straight
  // into the same client, so the app has to produce that shape whichever way
  // the user pasted it.
  test.each([
    ["https://homebox.example.com", "https://homebox.example.com/api"],
    ["https://homebox.example.com/api", "https://homebox.example.com/api"],
    ["https://homebox.example.com/api/v1", "https://homebox.example.com/api"],
    ["http://10.0.0.5:7745/", "http://10.0.0.5:7745/api"],
  ])("homebox %s -> %s", (raw, want) => {
    expect(normalizeServerUrl(raw, "homebox")).toEqual({ ok: true, value: want });
  });
});

describe("isLocalHost", () => {
  test.each([
    ["192.168.1.20", true],
    ["100.101.102.103", true], // a Tailscale IP is still an IP literal
    ["[fd00::1]", true],
    ["nas.local", true],
    ["nas", true],
    ["localhost", true],
    ["box.tailnet.ts.net", false], // MagicDNS: qualified, so iOS wants https
    ["box.example.com", false],
    ["", false],
  ])("%s -> %s", (host, want) => {
    expect(isLocalHost(host)).toBe(want);
  });

  test("hostOf strips port and user", () => {
    expect(hostOf("http://user@nas.local:8080/x")).toBe("nas.local");
    expect(hostOf("http://[fd00::1]:8080")).toBe("[fd00::1]");
  });
});

describe("transportWarnings", () => {
  test("https says nothing", () => {
    expect(transportWarnings(conn({ apiUrl: "https://box.example.com" }))).toEqual([]);
  });

  test("http to a LAN address warns about the token only", () => {
    const got = transportWarnings(conn({ apiUrl: "http://192.168.1.20:8080" }));
    expect(got).toHaveLength(1);
    expect(got[0]).toContain("unencrypted");
  });

  test.each(["http://127.0.0.1:8787", "http://localhost:8787", "http://[::1]:8787"])(
    "http to this phone itself says nothing: %s",
    (apiUrl) => {
      expect(transportWarnings(conn({ apiUrl }))).toEqual([]);
    },
  );

  test("http to a MagicDNS name says iOS will refuse it", () => {
    const got = transportWarnings(conn({ apiUrl: "http://box.tailnet.ts.net:8080" }));
    expect(got).toHaveLength(1);
    expect(got[0]).toContain("iOS only allows plain http://");
  });

  test("each server is judged on its own", () => {
    const got = transportWarnings(
      conn({ apiUrl: "https://box.example.com", homeboxUrl: "http://homebox.example.com/api" }),
    );
    expect(got).toHaveLength(1);
    expect(got[0]).toContain("Homebox");
  });
});

describe("validateConnection", () => {
  test("a complete self-hosted setup", () => {
    expect(
      validateConnection(conn({ apiUrl: " https://box.example.com/ ", apiToken: " Bearer abc123 " })),
    ).toEqual({ ok: true, connection: conn({ apiUrl: "https://box.example.com", apiToken: "abc123" }) });
  });

  // Mirrors credentialsFor in backend/internal/api/instance.go.
  test("a Homebox URL without its token is refused", () => {
    const got = validateConnection(conn({ apiUrl: "https://b.example.com", homeboxUrl: "https://hb.example.com" }));
    expect(got.ok).toBe(false);
    if (!got.ok) expect(got.errors.homeboxToken).toBeDefined();
  });

  test("a Homebox token alone is fine and reuses the backend's Homebox", () => {
    const got = validateConnection(conn({ apiUrl: "https://b.example.com", homeboxToken: "hb_x" }));
    expect(got).toEqual({ ok: true, connection: conn({ apiUrl: "https://b.example.com", homeboxToken: "hb_x" }) });
  });

  test("every problem is reported at once", () => {
    const got = validateConnection(conn({ apiUrl: "nope", homeboxUrl: "also nope", homeboxToken: "t" }));
    expect(got.ok).toBe(false);
    if (!got.ok) expect(Object.keys(got.errors).sort()).toEqual(["apiUrl", "homeboxUrl"]);
  });
});

describe("sameDestination", () => {
  const base = conn({ apiUrl: "https://b.example.com", apiToken: "one" });

  test("a rotated API token is the same inventory", () => {
    expect(sameDestination(base, { ...base, apiToken: "two" })).toBe(true);
  });

  test.each([
    ["another backend", { apiUrl: "https://other.example.com" }],
    ["another Homebox", { homeboxUrl: "https://hb.example.com/api", homeboxToken: "t" }],
    ["another Homebox account", { homeboxToken: "t" }],
  ])("%s is not", (_, patch) => {
    expect(sameDestination(base, { ...base, ...patch })).toBe(false);
  });
});

describe("persistence", () => {
  beforeEach(() => resetStore());

  test("save then load round-trips through the Keychain", async () => {
    const saved = conn({ apiUrl: "https://b.example.com", apiToken: "secret" });
    await saveConnection(saved);
    expect(JSON.parse(store.get(SERVICE) ?? "{}")).toEqual(saved);
    expect(await loadConnection()).toEqual(saved);
    expect(currentConnection()).toEqual(saved);
  });

  // A corrupt entry must land on the settings screen, not on every request.
  test.each([
    ["not JSON", "{{{"],
    ["an invalid address", JSON.stringify({ apiUrl: "ftp://x" })],
  ])("%s leaves the app unconfigured", async (_, raw) => {
    resetStore({ [SERVICE]: raw });
    const warn = jest.spyOn(console, "warn").mockImplementation(() => {});
    expect((await loadConnection()).apiUrl).toBe("");
    warn.mockRestore();
  });

  test("a Keychain that refuses to be read is not fatal", async () => {
    failNext();
    const warn = jest.spyOn(console, "warn").mockImplementation(() => {});
    expect((await loadConnection()).apiUrl).toBe("");
    expect(warn).toHaveBeenCalled();
    warn.mockRestore();
  });

  test("a failed save does not change the current connection", async () => {
    const before = currentConnection();
    failNext();
    await expect(saveConnection(conn({ apiUrl: "https://new.example.com" }))).rejects.toThrow();
    expect(currentConnection()).toEqual(before);
  });
});
