import { resetKv } from "../test/react-native-mmkv";
import {
  defaultUnits,
  dimsFieldText,
  formatDimsIn,
  formatVolume,
  parseCapacityIn,
  parseDimsIn,
  resetUnitsForTest,
  setUnits,
  toggleUnits,
  units,
} from "./units";

beforeEach(() => {
  resetKv();
  resetUnitsForTest();
});

describe("which system", () => {
  test("the region decides until someone chooses", () => {
    expect(defaultUnits("en-US")).toBe("imperial");
    expect(defaultUnits("en_US")).toBe("imperial");
    expect(defaultUnits("en-GB")).toBe("metric");
    expect(defaultUnits("fr-CA")).toBe("metric");
    expect(defaultUnits("en")).toBe("metric");
    expect(defaultUnits("")).toBe("metric");
  });

  test("a choice is remembered across a restart", () => {
    setUnits("imperial");
    resetUnitsForTest();
    expect(units()).toBe("imperial");
    toggleUnits();
    resetUnitsForTest();
    expect(units()).toBe("metric");
  });
});

describe("showing", () => {
  const drill = { l: 25, w: 20, h: 8 };

  test("sizes in either system", () => {
    expect(formatDimsIn(drill, "metric")).toBe("25 × 20 × 8 cm");
    expect(formatDimsIn(drill, "imperial")).toBe("9.8 × 7.9 × 3.1 in");
    expect(dimsFieldText(drill, "imperial")).toBe("9.8 x 7.9 x 3.1");
  });

  test("capacities as they are sold", () => {
    expect(formatVolume(102, "metric")).toBe("102 L");
    expect(formatVolume(102, "imperial")).toBe("26.9 gal");
    expect(formatVolume(68, "imperial")).toBe("18 gal");
  });
});

describe("reading what someone typed", () => {
  test("a bare size is in the chosen system", () => {
    expect(parseDimsIn("30 x 20 x 10", "metric")).toEqual({ l: 30, w: 20, h: 10 });
    expect(parseDimsIn("12 x 8 x 4", "imperial")).toEqual({ l: 30.5, w: 20.3, h: 10.2 });
  });

  test("a unit written in wins, either way round", () => {
    expect(parseDimsIn("12 x 8 x 4 in", "metric")).toEqual({ l: 30.5, w: 20.3, h: 10.2 });
    expect(parseDimsIn('12" x 8" x 4"', "metric")).toEqual({ l: 30.5, w: 20.3, h: 10.2 });
    expect(parseDimsIn("30 x 20 x 10 cm", "imperial")).toEqual({ l: 30, w: 20, h: 10 });
    expect(parseDimsIn("300 x 200 x 100 mm", "imperial")).toEqual({ l: 30, w: 20, h: 10 });
    expect(parseDimsIn("20 × 30 × 10", "metric")).toEqual({ l: 30, w: 20, h: 10 });
  });

  test("nonsense is no size", () => {
    expect(parseDimsIn("30 x 20", "metric")).toBeUndefined();
    expect(parseDimsIn("a x b x c", "metric")).toBeUndefined();
    expect(parseDimsIn("0 x 20 x 10", "metric")).toBeUndefined();
    expect(parseDimsIn("200 x 20 x 10", "imperial")).toBeUndefined(); // 5 m
    expect(parseDimsIn("", "metric")).toBeUndefined();
  });

  test("a bare capacity is litres or US gallons by the chosen system", () => {
    expect(parseCapacityIn("100", "metric")).toBe(100);
    expect(parseCapacityIn("27", "imperial")).toBe(102);
    expect(parseCapacityIn("27 gal", "metric")).toBe(102);
    expect(parseCapacityIn("100 L", "imperial")).toBe(100);
    expect(parseCapacityIn("27 quarts", "imperial")).toBeUndefined();
    expect(parseCapacityIn("0", "metric")).toBeUndefined();
  });
});
