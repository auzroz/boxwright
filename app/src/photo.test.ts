/**
 * The intake point every photo goes through, whatever it came from. Both
 * native modules are replaced here rather than in test/: nothing else imports
 * them, and what matters is only what each one is asked to do.
 */
const mockFiles = new Map<string, string>();
let mockResizeFails = false;

jest.mock("@react-native-community/image-editor", () => ({
  __esModule: true,
  default: {
    cropImage: jest.fn(async (uri: string, data: { displaySize: { width: number; height: number } }) => {
      if (mockResizeFails) throw new Error("unsupported image");
      const path = `/cache/cropped-${mockFiles.size}.jpg`;
      mockFiles.set(path, `resized(${uri.slice(0, 16)}) to ${data.displaySize.width}x${data.displaySize.height}`);
      return { path, uri: `file://${path}` };
    }),
  },
}));

// Only Image.getSize is used; the real module cannot load under node.
jest.mock("react-native", () => ({
  Image: {
    getSize: (_uri: string, ok: (w: number, h: number) => void) => ok(4032, 3024),
  },
}));

jest.mock("react-native-file-access", () => ({
  Dirs: { DocumentDir: "/doc" },
  FileSystem: {
    exists: jest.fn(async (path: string) => path === "/doc/captures" || mockFiles.has(path)),
    mkdir: jest.fn(async () => {}),
    mv: jest.fn(async (from: string, to: string) => {
      const data = mockFiles.get(from);
      if (data === undefined) throw new Error(`no such file: ${from}`);
      mockFiles.delete(from);
      mockFiles.set(to, data);
    }),
    cp: jest.fn(async (from: string, to: string) => {
      const data = mockFiles.get(from);
      if (data === undefined) throw new Error(`no such file: ${from}`);
      mockFiles.set(to, data);
    }),
    unlink: jest.fn(async (path: string) => {
      mockFiles.delete(path);
    }),
    ls: jest.fn(async () => []),
  },
}));

import { downscaledSize, isFileUri, persistCaptureDepth, persistCapturePhoto } from "./photo";

beforeEach(() => {
  mockFiles.clear();
  mockResizeFails = false;
});

describe("taking a photo in from any source", () => {
  test("a file source becomes a durable, downscaled copy", async () => {
    mockFiles.set("/tmp/picked.heic", "original");
    const stored = await persistCapturePhoto("cap1", "file:///tmp/picked.heic");
    expect(stored).toEqual({ name: "cap1.jpg", uri: "file:///doc/captures/cap1.jpg", downscaled: true });
    expect(mockFiles.get("/doc/captures/cap1.jpg")).toMatch(/^resized.* to 1024x768$/);
  });

  test("a pasted data uri becomes a file like any other", async () => {
    const stored = await persistCapturePhoto("cap2", "data:image/png;base64,iVBORw0KGgo=");
    expect(stored.name).toBe("cap2.jpg");
    expect(stored.downscaled).toBe(true);
    expect(mockFiles.has("/doc/captures/cap2.jpg")).toBe(true);
  });

  test("a file that will not downscale is kept as it lies", async () => {
    mockResizeFails = true;
    mockFiles.set("/tmp/picked.jpg", "original");
    const stored = await persistCapturePhoto("cap3", "file:///tmp/picked.jpg");
    expect(stored.downscaled).toBe(false);
    expect(mockFiles.get("/doc/captures/cap3.jpg")).toBe("original");
    // Copied, not moved: the picker's file is not ours to take.
    expect(mockFiles.has("/tmp/picked.jpg")).toBe(true);
  });

  test("a kept original keeps its extension, so it is not uploaded as a JPEG", async () => {
    mockResizeFails = true;
    mockFiles.set("/tmp/picked.heic", "original");
    const stored = await persistCapturePhoto("cap5", "file:///tmp/picked.heic");
    expect(stored).toEqual({ name: "cap5.heic", uri: "file:///doc/captures/cap5.heic", downscaled: false });
  });

  test("a pasted image that will not decode is refused, not faked", async () => {
    mockResizeFails = true;
    await expect(persistCapturePhoto("cap4", "data:image/jpeg;base64,bm90YW5pbWFnZQ==")).rejects.toThrow(
      /pasted image/,
    );
    expect(mockFiles.size).toBe(0);
  });

  test("isFileUri tells the two apart", () => {
    expect(isFileUri("file:///tmp/a.jpg")).toBe(true);
    expect(isFileUri("data:image/png;base64,AAAA")).toBe(false);
  });
});

describe("keeping a depth file", () => {
  test("it moves beside the photo as <captureId>.depth, from a path or a file uri", async () => {
    mockFiles.set("/tmp/ar-1.depth", "depth bytes");
    expect(await persistCaptureDepth("cap1", "/tmp/ar-1.depth")).toBe("cap1.depth");
    expect(mockFiles.get("/doc/captures/cap1.depth")).toBe("depth bytes");
    // Moved: the temporary directory is not somewhere a parked capture can wait.
    expect(mockFiles.has("/tmp/ar-1.depth")).toBe(false);

    mockFiles.set("/tmp/ar 2.depth", "more");
    expect(await persistCaptureDepth("cap2", "file:///tmp/ar%202.depth")).toBe("cap2.depth");
    expect(mockFiles.get("/doc/captures/cap2.depth")).toBe("more");
  });

  test("a file that is not there is an error, not a name for nothing", async () => {
    await expect(persistCaptureDepth("cap3", "/tmp/missing.depth")).rejects.toThrow(/no such file/);
  });
});

describe("the size a photo is re-encoded at", () => {
  test("a phone photo comes down to 1024 on its long edge, either way up", () => {
    expect(downscaledSize(4032, 3024)).toEqual({ width: 1024, height: 768 });
    expect(downscaledSize(3024, 4032)).toEqual({ width: 768, height: 1024 });
  });

  test("a small image is re-encoded at its own size, never scaled up", () => {
    expect(downscaledSize(640, 480)).toEqual({ width: 640, height: 480 });
  });

  test("a sliver keeps at least a pixel", () => {
    expect(downscaledSize(100000, 10)).toEqual({ width: 1024, height: 1 });
  });
});
