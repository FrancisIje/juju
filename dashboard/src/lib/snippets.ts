// Setup code shown to new projects, with the project's DSN filled in, so
// connecting an app is a copy and paste.

export type Platform = "web" | "react-native" | "flutter" | "curl";

export const platforms: { id: Platform; label: string }[] = [
  { id: "web", label: "Web" },
  { id: "react-native", label: "React Native" },
  { id: "flutter", label: "Flutter" },
  { id: "curl", label: "curl" },
];

export function snippet(p: Platform, dsn: string): { install?: string; code: string } {
  switch (p) {
    case "web":
      return {
        install: "npm install @juju/web",
        code: `import * as juju from "@juju/web";

juju.init({
  dsn: "${dsn}",
  release: "web@1.0.0",
});`,
      };
    case "react-native":
      return {
        install: "npm install @juju/react-native @react-native-async-storage/async-storage",
        code: `import AsyncStorage from "@react-native-async-storage/async-storage";
import * as juju from "@juju/react-native";

juju.init({
  dsn: "${dsn}",
  release: "myapp@1.0.0+1",
  storage: AsyncStorage, // keeps crashes until they're sent
});`,
      };
    case "flutter":
      return {
        install: "flutter pub add juju_flutter",
        code: `import 'package:flutter/material.dart';
import 'package:juju_flutter/juju_flutter.dart';

void main() => Juju.runZoned(() async {
      WidgetsFlutterBinding.ensureInitialized();
      await Juju.init(JujuOptions(dsn: '${dsn}', release: 'myapp@1.0.0+1'));
      runApp(const MyApp());
    });`,
      };
    case "curl": {
      const m = /^(https?):\/\/([^@]+)@([^/]+)\/(\d+)/.exec(dsn);
      const [, scheme, key, host, id] = m ?? [];
      return {
        code: `curl -X POST "${scheme}://${host}/api/v1/projects/${id}/events" \\
  -H "Content-Type: application/json" -H "X-Juju-Key: ${key}" \\
  -d '{"sdk":{"name":"curl"},"events":[{"event_id":"'$(uuidgen)'","type":"error",
       "platform":"javascript","exception":{"type":"TypeError","message":"hello from curl"}}]}'`,
      };
    }
  }
}

/** How to send an analytics event, per platform, shown before the first one arrives. */
export function captureSnippet(p: Platform, dsn: string): string {
  switch (p) {
    case "web":
      return `import * as juju from "@juju/web";

juju.capture("checkout_started", { plan: "pro", items: 3 });

// After login and logout:
juju.identify(user.id);
juju.reset();`;
    case "react-native":
      return `import * as juju from "@juju/react-native";

juju.capture("checkout_started", { plan: "pro", items: 3 });

// After login and logout:
juju.identify(user.id);
juju.reset();`;
    case "flutter":
      return `import 'package:juju_flutter/juju_flutter.dart';

Juju.capture('checkout_started', {'plan': 'pro', 'items': 3});

// After login and logout:
Juju.identify(user.id);
Juju.reset();`;
    case "curl": {
      const m = /^(https?):\/\/([^@]+)@([^/]+)\/(\d+)/.exec(dsn);
      const [, scheme, key, host, id] = m ?? [];
      return `curl -X POST "${scheme}://${host}/api/v1/projects/${id}/events" \\
  -H "Content-Type: application/json" -H "X-Juju-Key: ${key}" \\
  -d '{"sdk":{"name":"curl"},"events":[{"event_id":"'$(uuidgen)'","type":"event",
       "name":"checkout_started","distinct_id":"user_42","properties":{"plan":"pro"}}]}'`;
    }
  }
}
