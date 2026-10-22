# Symbolication fixtures

Real files from real tools, so the tests check against what builds actually produce.
Regenerate them the same way if a tool's output format changes.

## JavaScript: `app.min.js`, `app.min.js.map`, `app.min.js.stack.txt`

A small `app.js` (see the map's `sourcesContent`) minified with esbuild, run in Node to get a real V8 stack:

```bash
esbuild app.js --bundle --minify --sourcemap --format=esm --outfile=out/app.min.js
node -e 'import("./out/app.min.js").then(m => console.log(m.run()))'
```

The stack's `file://` paths were replaced with `https://shop.example.com/assets/`.

## Dart on macOS: `dart_macos_arm64.*` (Mach-O)

```bash
dart compile aot-snapshot --extra-gen-snapshot-options=--dwarf-stack-traces,--save-debugging-info=shop.debug -o shop.aot bin/shop.dart
dartaotruntime shop.aot > trace.txt
dart run native_stack_traces:decode translate -d shop.debug -i trace.txt > decoded.txt   # the answer key
```

## Flutter on Android: `flutter_android_arm64.*` (ELF)

From `examples/flutter`, built with `flutter build apk --release --obfuscate --split-debug-info=build/symbols`
and run on the Android emulator. The trace is copied from `adb logcat` (two traces: an async error and a
zone error); the decoded answer key comes from `native_stack_traces` as above. This file's line table has
sequences out of address order, which is what broke a forward-scanning lookup.

## JavaScript, several lines: `multiline.js`, `multiline.js.map`

For the debug id tests, which shift every line by one: an unminified esbuild bundle of two small files
(`cart.js` and `main.js`, in the map's `sourcesContent`), so there are many lines to check.

```bash
esbuild main.js --bundle --sourcemap --format=iife --outfile=multiline.js
```
