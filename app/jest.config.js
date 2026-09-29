/**
 * Tier 1: the app's own logic, with nothing native in the process.
 *
 * `testEnvironment: node` and no React Native jest preset on purpose. A
 * rendering environment is only needed to RENDER components, it still cannot
 * load the native modules below without the same fakes, and it costs a great
 * deal to install. What this covers is where the app can lose a capture: the
 * offline queue, the category fold, the offline picker, and the connection.
 */
module.exports = {
  testEnvironment: "node",
  roots: ["<rootDir>/src", "<rootDir>/test"],
  testMatch: ["**/*.test.ts"],
  transform: { "^.+\\.[jt]sx?$": ["babel-jest", { configFile: "./babel.config.js" }] },
  moduleNameMapper: {
    // All three reach native code the moment they are imported.
    "^react-native-mmkv$": "<rootDir>/test/react-native-mmkv.ts",
    "^react-native-keychain$": "<rootDir>/test/react-native-keychain.ts",
    // The local Nitro module (modules/boxwright-depth): a HybridObject and a
    // Fabric view. Mapped by name so app code can import it as the package.
    "^boxwright-depth$": "<rootDir>/test/boxwright-depth.ts",
  },
  clearMocks: true,
};
