# juju Flutter example

A Flutter app using `juju_flutter` from `../../sdks/flutter` (a path dependency, so SDK changes are picked up
on the next build). Each button causes a different kind of error.

```bash
flutter run                      # debug, on a simulator or emulator
flutter build apk --release      # release: Android only on emulators (Flutter has no release mode on the iOS simulator)
```

At launch the app asks `http://localhost:8098/config.json` for `{"dsn": "...", "scenario": "none" | "all"}`.
`all` triggers a framework error, an async error, a zone error and a handled error. See the `/verify` skill.
On Android, run `adb reverse tcp:8080 tcp:8080` and `adb reverse tcp:8098 tcp:8098` first.

The app allows plain `http://` to localhost (`AndroidManifest.xml`, `Info.plist`) so it can reach a local juju
server. Real apps use https and must not copy that.
