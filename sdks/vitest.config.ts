import { fileURLToPath } from "node:url";
import { defineConfig } from "vitest/config";

export default defineConfig({
  resolve: {
    // Tests import @juju/core from source, so a change in core is tested
    // immediately without rebuilding it.
    alias: {
      "@juju/core": fileURLToPath(new URL("./js-core/src/index.ts", import.meta.url)),
      // React Native itself can't run in Node, so its tests get a small fake.
      "react-native": fileURLToPath(new URL("./react-native/test/react-native-fake.ts", import.meta.url)),
    },
  },
  test: {
    // .test.js: tests of plain-JavaScript files that run in Node, like the
    // Metro plugin, which the TypeScript check (browser types only) skips.
    include: ["*/test/**/*.test.ts", "*/test/**/*.test.js"],
    // Core must not depend on the browser, so it runs in plain Node. Web
    // tests opt into a simulated browser with a comment at the top of the
    // file: // @vitest-environment happy-dom
    environment: "node",
    restoreMocks: true,
  },
});
