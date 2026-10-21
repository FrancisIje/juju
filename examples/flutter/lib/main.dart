// juju Flutter example: each button causes a different kind of error, or
// sends a product analytics event.
//
// For automated checks, the app asks a small control server on the
// development machine (port 8098) for its DSN and a scenario to run at
// launch, so an emulator can be driven without tapping the screen. Without
// the control server, the app shows how to set one up.

import 'dart:async';
import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:http/http.dart' as http;
import 'package:juju_flutter/juju_flutter.dart';

const controlUrl = 'http://localhost:8098/config.json';
const release = 'juju-flutter-example@0.1.0+1';

// Everything, including the Flutter binding and runApp, runs inside
// Juju.runZoned, so they share one zone and zone errors are reported.
void main() => Juju.runZoned(() async {
  WidgetsFlutterBinding.ensureInitialized();
  final config = await loadConfig();
  if (config != null) {
    await Juju.init(
      JujuOptions(
        dsn: config.dsn,
        release: release,
        appPackages: const {'juju_example'},
        debug: true,
        flushInterval: const Duration(seconds: 1),
      ),
    );
    Juju.setUser({'id': 'example-user'});
  }
  runApp(
    ExampleApp(
      scenario: config?.scenario ?? 'none',
      configured: config != null,
    ),
  );
});

class Config {
  Config(this.dsn, this.scenario);
  final String dsn;
  final String scenario;
}

Future<Config?> loadConfig() async {
  try {
    final res = await http
        .get(Uri.parse(controlUrl))
        .timeout(const Duration(seconds: 2));
    if (res.statusCode != 200) return null;
    final json = jsonDecode(res.body) as Map<String, dynamic>;
    return Config(
      json['dsn'] as String,
      (json['scenario'] as String?) ?? 'none',
    );
  } catch (_) {
    return null;
  }
}

final api = Juju.httpClient(); // records each request as a breadcrumb

class Cart {
  Cart(this.items, [this.coupon]);
  final List<int> items;
  final Map<String, int>? coupon;
}

// A realistic bug: the coupon API failed, so cart.coupon is null.
int checkout(Cart cart) =>
    cart.items.fold(0, (a, b) => a + b) - cart.coupon!['amount']!;

final scenarios = <String, void Function(BuildContext)>{
  // Thrown while building a widget: Flutter shows the error screen (grey in
  // release) and reports it through FlutterError.onError.
  'framework': (context) => Navigator.of(context).pushNamed('/broken-cart'),
  // An async error in the root zone, outside Juju.runZoned: nobody awaits
  // the future, so it reaches PlatformDispatcher.onError.
  'async': (_) => Zone.root.run(
    () => Future<void>.delayed(
      Duration.zero,
      () => throw StateError('Checkout API failed: 503'),
    ),
  ),
  // An async error inside the app's zone goes to the zone's handler.
  'zone': (_) => Timer.run(() => checkout(Cart([10]))),
  'captured': (_) {
    try {
      jsonDecode('{not json');
    } catch (e, s) {
      Juju.captureException(e, s, {'step': 'parse-config'});
    }
  },
  // Product analytics: events, not errors.
  'checkout': (_) =>
      Juju.capture('checkout_started', {'plan': 'pro', 'items': 3}),
  'completed': (_) => Juju.capture('checkout_completed', {'plan': 'pro'}),
  'login': (_) => Juju.identify('flutter_user_1'),
  'logout': (_) => Juju.reset(),
  'breadcrumbs': (_) {
    print('coupon request is about to fail'); // ignore: avoid_print
    api
        .get(Uri.parse('http://localhost:8098/api/coupon?token=never-recorded'))
        .ignore();
  },
};

class ExampleApp extends StatelessWidget {
  const ExampleApp({
    super.key,
    required this.scenario,
    required this.configured,
  });
  final String scenario;
  final bool configured;

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      title: 'juju Flutter example',
      theme: ThemeData(
        colorSchemeSeed: const Color(0xFFB45309),
        brightness: Brightness.dark,
      ),
      navigatorObservers: [Juju.navigatorObserver],
      routes: {
        '/': (_) => HomePage(scenario: scenario, configured: configured),
        '/broken-cart': (_) => const BrokenCartPage(),
        '/cart': (_) => const CartPage(),
      },
    );
  }
}

class HomePage extends StatefulWidget {
  const HomePage({super.key, required this.scenario, required this.configured});
  final String scenario;
  final bool configured;

  @override
  State<HomePage> createState() => _HomePageState();
}

class _HomePageState extends State<HomePage> {
  final log = <String>[];

  void add(String line) =>
      setState(() => log.add('${TimeOfDay.now().format(context)}  $line'));

  @override
  void initState() {
    super.initState();
    WidgetsBinding.instance.addPostFrameCallback((_) => runLaunchScenario());
  }

  Future<void> runLaunchScenario() async {
    if (!widget.configured) {
      add(
        'No control server: serve {"dsn": "http://pk_...@localhost:8080/1"} as config.json on port 8098.',
      );
      return;
    }
    add('juju initialised (scenario: ${widget.scenario})');
    if (widget.scenario == 'screens') {
      // A healthy screen load: open the cart (it fetches its items), come
      // back, and send. The cart's load and its request become a
      // screen.load transaction with an http.client span.
      await Future<void>.delayed(const Duration(milliseconds: 500));
      if (!mounted) return;
      Navigator.of(context).pushNamed('/cart');
      await Future<void>.delayed(const Duration(seconds: 3));
      if (!mounted) return;
      Navigator.of(context).pop();
      await Juju.flush();
      add('opened and closed the cart');
      return;
    }
    if (widget.scenario == 'analytics') {
      // A known set, to check a chart against: 2 checkout_started (one
      // before login, one after), an identify, 1 checkout_completed.
      for (final name in ['checkout', 'login', 'checkout', 'completed']) {
        scenarios[name]!(context);
      }
      await Juju.flush();
      add('analytics scenario sent');
      return;
    }
    if (widget.scenario != 'all') return;
    scenarios['breadcrumbs']!(context);
    await Future<void>.delayed(const Duration(milliseconds: 300));
    if (!mounted) return;
    for (final name in ['captured', 'async', 'zone']) {
      scenarios[name]!(context);
    }
    await Future<void>.delayed(const Duration(milliseconds: 300));
    if (mounted) scenarios['framework']!(context);
    add('all scenarios triggered');
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(title: const Text('juju Flutter example')),
      body: ListView(
        padding: const EdgeInsets.all(16),
        children: [
          for (final entry in {
            'framework': 'Framework error (widget build)',
            'async': 'Async error (unawaited future)',
            'zone': 'Zone error (timer)',
            'captured': 'Handled error (captureException)',
            'breadcrumbs': 'Breadcrumbs (print + http)',
            'checkout': 'Start checkout (capture)',
            'completed': 'Complete checkout (capture)',
            'login': 'Log in as flutter_user_1 (identify)',
            'logout': 'Log out (reset)',
          }.entries)
            Padding(
              padding: const EdgeInsets.only(bottom: 8),
              child: FilledButton.tonal(
                onPressed: () {
                  scenarios[entry.key]!(context);
                  add('triggered ${entry.key}');
                },
                child: Text(entry.value),
              ),
            ),
          OutlinedButton(
            onPressed: () => Juju.flush().then((_) => add('flushed')),
            child: const Text('Send now'),
          ),
          const SizedBox(height: 16),
          Text(
            log.join('\n'),
            style: const TextStyle(fontFamily: 'monospace', fontSize: 12),
          ),
        ],
      ),
    );
  }
}

class BrokenCartPage extends StatelessWidget {
  const BrokenCartPage({super.key});

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(title: const Text('Cart')),
      body: Text('Total: ${checkout(Cart([10]))}'),
    );
  }
}

/// A screen that loads its data when it opens, like most real screens.
class CartPage extends StatefulWidget {
  const CartPage({super.key});

  @override
  State<CartPage> createState() => _CartPageState();
}

class _CartPageState extends State<CartPage> {
  String status = 'loading…';

  @override
  void initState() {
    super.initState();
    api
        .get(Uri.parse('http://localhost:8098/config.json?cart=1'))
        .then((r) {
          if (mounted) setState(() => status = 'loaded (${r.statusCode})');
        })
        .catchError((Object e) {
          if (mounted) setState(() => status = 'failed: $e');
        });
  }

  @override
  Widget build(BuildContext context) => Scaffold(
    appBar: AppBar(title: const Text('Cart')),
    body: Center(child: Text(status)),
  );
}
