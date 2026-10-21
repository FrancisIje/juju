# juju React Native example

A React Native 0.87 app with `@juju/react-native` installed. Each button causes a different kind of error.

## Run it

```bash
make example-rn-deps        # from the repo root: build + pack the SDKs, install them here
cd examples/react-native
npx react-native run-ios     # or: npx react-native run-android
```

Paste a DSN (`http://<public_key>@localhost:8080/<project id>`) into the app and tap **Save DSN**, then restart it.
On Android, run `adb reverse tcp:8080 tcp:8080` first: inside the emulator, `localhost` is the emulator itself.

## Why the SDK is installed from tarballs

`package.json` points at `vendor/*.tgz`, made by `npm pack`. That installs exactly the files an
npm user would get, so a file missing from the published package shows up here first.
After changing the SDK, run `make example-rn-deps` again.

## Automated runs

At launch the app asks `http://localhost:8098/config.json` for `{"dsn": "...", "scenario": "none" | "nonfatal" | "crash" | "analytics"}`.
`analytics` sends a known set of product analytics events (2 `checkout_started`, one before and one after
`identify("rn_user_1")`), so a chart can be checked against exactly what was sent.
Serving that file from any folder (`python3 -m http.server 8098`) lets a script crash the app on one launch and
reopen it normally on the next, without tapping the screen. See the `/verify` skill.

The Android release build allows plain `http://` (see `android/app/build.gradle`) so it can reach the local
server. Real apps use https and must not copy that line.
