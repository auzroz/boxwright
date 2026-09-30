/**
 * Request construction and the connection probe.
 *
 * `fetch` is replaced per test. What is pinned: the headers a connection turns
 * into (the Homebox pair only when set, because a backend that does not accept
 * client credentials refuses a request carrying them), that an unconfigured app
 * queues rather than discards, and that each way a probe can fail comes back
 * as the sentence that says how to fix it.
 */
import { resetStore } from "../test/react-native-keychain";

import { ApiError, connectionHeaders, imageTypeFor, isRetriable, probe, recommend } from "./api";
import { EMPTY_CONNECTION, saveConnection } from "./connection";
import type { Connection } from "./connection";
import type { StatusResponse } from "./types";

const server: Connection = { ...EMPTY_CONNECTION, apiUrl: "https://box.example.com", apiToken: "s3cret" };

const healthy: StatusResponse = {
  version: "0.1.0",
  apiVersion: 1,
  identification: true,
  aiProvider: "anthropic",
  clientHomebox: false,
  homebox: { ok: true },
};

function respond(status: number, body: unknown): jest.Mock {
  const mock = jest.fn(async () => new Response(JSON.stringify(body), { status }));
  globalThis.fetch = mock as unknown as typeof fetch;
  return mock;
}

function sentHeaders(mock: jest.Mock): Headers {
  const init = mock.mock.calls[0]?.[1] as RequestInit | undefined;
  return new Headers(init?.headers);
}

const realFetch = globalThis.fetch;
afterEach(() => {
  globalThis.fetch = realFetch;
});

describe("connectionHeaders", () => {
  test("the self-hosted default sends only the API token", () => {
    const h = connectionHeaders(server);
    expect(h.get("Authorization")).toBe("Bearer s3cret");
    expect(h.has("X-Boxwright-Homebox-Url")).toBe(false);
    expect(h.has("X-Boxwright-Homebox-Token")).toBe(false);
  });

  test("a Homebox of one's own sends both", () => {
    const h = connectionHeaders({ ...server, homeboxUrl: "https://hb.example.com/api", homeboxToken: "hb_x" });
    expect(h.get("X-Boxwright-Homebox-Url")).toBe("https://hb.example.com/api");
    expect(h.get("X-Boxwright-Homebox-Token")).toBe("hb_x");
  });

  test("no token, no Authorization header", () => {
    expect(connectionHeaders({ ...server, apiToken: "" }).has("Authorization")).toBe(false);
  });

  test("the caller's own headers are kept", () => {
    expect(connectionHeaders(server, { "Content-Type": "application/json" }).get("Content-Type")).toBe(
      "application/json",
    );
  });
});

describe("requests follow the saved connection", () => {
  beforeEach(() => resetStore());

  // A capture taken before setup must wait for a server, not be thrown away.
  test("with no server configured, a call fails as retriable without touching the network", async () => {
    await saveConnection(EMPTY_CONNECTION);
    const mock = respond(200, {});
    const err = await recommend([]).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect(isRetriable(err)).toBe(true);
    expect(mock).not.toHaveBeenCalled();
  });

  test("a saved change applies to the very next request", async () => {
    await saveConnection(server);
    let mock = respond(200, { recommendations: [], stale: false });
    await recommend([]);
    expect(mock.mock.calls[0]?.[0]).toBe("https://box.example.com/api/v1/recommend");

    await saveConnection({ ...server, apiUrl: "https://other.example.com", apiToken: "new" });
    mock = respond(200, { recommendations: [], stale: false });
    await recommend([]);
    expect(mock.mock.calls[0]?.[0]).toBe("https://other.example.com/api/v1/recommend");
    expect(sentHeaders(mock).get("Authorization")).toBe("Bearer new");
  });
});

describe("probe", () => {
  test("tests the connection it is given, not the saved one", async () => {
    await saveConnection({ ...server, apiUrl: "https://saved.example.com" });
    const mock = respond(200, healthy);
    const got = await probe(server);
    expect(mock.mock.calls[0]?.[0]).toBe("https://box.example.com/api/v1/status");
    expect(sentHeaders(mock).get("Authorization")).toBe("Bearer s3cret");
    expect(got).toEqual({ ok: true, status: healthy, warnings: [] });
  });

  test.each([
    [401, { error: "missing or invalid API token" }, "did not accept the token"],
    [404, "404 page not found", "older one without /api/v1/status"],
    [400, { error: "this server does not accept client Homebox credentials" }, "does not accept client"],
  ])("HTTP %i explains itself", async (status, body, message) => {
    respond(status, body);
    const got = await probe(server);
    expect(got.ok).toBe(false);
    if (!got.ok) expect(got.message).toContain(message);
  });

  test("an unreachable server says where to look", async () => {
    globalThis.fetch = jest.fn(async () => {
      throw new TypeError("Network request failed");
    }) as unknown as typeof fetch;
    const got = await probe(server);
    expect(got.ok).toBe(false);
    if (!got.ok) expect(got.message).toContain("tailnet");
  });

  test.each([
    ["Homebox is down", { homebox: { ok: false, error: "dial tcp: refused" } }, "cannot reach Homebox: dial tcp"],
    ["the server is newer", { apiVersion: 2 }, "newer than this app"],
  ])("a working server with a caveat: %s", async (_, patch, message) => {
    respond(200, { ...healthy, ...patch });
    const got = await probe(server);
    expect(got.ok).toBe(true);
    if (got.ok) expect(got.warnings.join(" ")).toContain(message);
  });

  // The screen lists it among what works ("Identification is off. You'll
  // enter items by hand."); a warning saying it again read as a problem.
  test("identification being off is reported by status, not as a warning", async () => {
    respond(200, { ...healthy, identification: false, aiProvider: "none" });
    const got = await probe(server);
    expect(got.ok && got.status.identification).toBe(false);
    if (got.ok) expect(got.warnings).toEqual([]);
  });
});

// The backend hands the part's type straight to the vision model, and the
// stricter ones refuse a PNG labelled as JPEG.
describe("imageTypeFor", () => {
  test.each([
    ["file:///doc/captures/abc.jpg", "image/jpeg"],
    ["file:///doc/captures/abc.JPEG", "image/jpeg"],
    ["file:///tmp/rn_image_picker_lib_temp_1.png", "image/png"],
    ["file:///doc/captures/abc.gif", "image/gif"],
    ["file:///doc/captures/abc.heic", "image/heic"],
    ["file:///doc/captures/no-extension", "image/jpeg"],
  ])("%s -> %s", (uri, want) => {
    expect(imageTypeFor(uri)).toBe(want);
  });
});
