import { nextStep, previousStep, startingStep, stepLabel } from "./setup";

describe("where setup starts", () => {
  test("a fresh install starts at the beginning", () => {
    expect(startingStep({}, false)).toBe("welcome");
  });

  // The maintainer's phone today: connected, locations chosen, no setup record.
  test("someone already using the app is never sent through it", () => {
    expect(startingStep({ units: "imperial" }, true)).toBe("done");
  });

  test("finished stays finished, connection or not", () => {
    expect(startingStep({ setupDone: true }, true)).toBe("done");
    expect(startingStep({ setupDone: true }, false)).toBe("done");
  });

  test("a relaunch resumes where it left off", () => {
    expect(startingStep({ setupStep: "sizes" }, true)).toBe("sizes");
    expect(startingStep({ setupStep: "welcome" }, false)).toBe("welcome");
  });

  test("a step that needs a server goes back to connect when there is none", () => {
    expect(startingStep({ setupStep: "locations" }, false)).toBe("connect");
    expect(startingStep({ setupStep: "ready" }, false)).toBe("connect");
  });

  test("a step name from some other version is ignored", () => {
    expect(startingStep({ setupStep: "billing" }, false)).toBe("welcome");
    expect(startingStep({ setupStep: "billing" }, true)).toBe("done");
  });
});

test("steps run in order and end in the app", () => {
  expect(nextStep("welcome")).toBe("connect");
  expect(nextStep("sizes")).toBe("prefs");
  expect(nextStep("ready")).toBe("done");
  expect(previousStep("connect")).toBe("welcome");
  expect(previousStep("welcome")).toBeUndefined();
});

test("only the middle four are numbered", () => {
  expect(stepLabel("welcome")).toBeUndefined();
  expect(stepLabel("connect")).toEqual({ index: 1, count: 4 });
  expect(stepLabel("prefs")).toEqual({ index: 4, count: 4 });
  expect(stepLabel("ready")).toBeUndefined();
});
