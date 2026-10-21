// juju React Native example: each button causes a different kind of error.
//
// For automated checks, the app also asks a small control server on the
// development machine (port 8098) for its DSN and a scenario to run at
// launch, so a simulator can be told "crash this time" without anyone
// tapping the screen. Without the control server, paste a DSN by hand.

import AsyncStorage from '@react-native-async-storage/async-storage';
import * as juju from '@juju/react-native';
import React, { useEffect, useState } from 'react';
import { Button, SafeAreaView, ScrollView, StyleSheet, Text, TextInput, View } from 'react-native';

const CONTROL_URL = 'http://localhost:8098/config.json';
const DSN_KEY = 'juju-example-dsn';
const RELEASE = 'juju-example@0.1.0+1';

type Config = { dsn: string; scenario?: 'none' | 'crash' | 'nonfatal' | 'analytics' };

async function loadConfig(): Promise<Config | null> {
  try {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 2000);
    const res = await fetch(CONTROL_URL, { signal: controller.signal });
    clearTimeout(timer);
    if (res.ok) return (await res.json()) as Config;
  } catch {
    // no control server: fall back to the DSN saved last time
  }
  const dsn = await AsyncStorage.getItem(DSN_KEY);
  return dsn ? { dsn, scenario: 'none' } : null;
}

function checkout(cart: { items: { price: number }[]; coupon?: { amount: number } }) {
  // A realistic bug: the coupon API failed, so cart.coupon is undefined.
  return cart.items.reduce((sum, item) => sum + item.price, 0) - cart.coupon!.amount;
}

const scenarios = {
  // Thrown from a timer, so nothing catches it: React Native treats it as
  // fatal, and a release build closes the app.
  crash: () => setTimeout(() => checkout({ items: [{ price: 10 }] }), 0),
  rejection: () => {
    Promise.reject(new Error('Checkout API failed: 503'));
  },
  captured: () => {
    try {
      JSON.parse('{not json');
    } catch (err) {
      juju.captureException(err, { tags: { step: 'parse-config' } });
    }
  },
  // Product analytics: events, not errors.
  checkout: () => juju.capture('checkout_started', { plan: 'pro', items: 3 }),
  completed: () => juju.capture('checkout_completed', { plan: 'pro' }),
  login: () => juju.identify('rn_user_1'),
  logout: () => juju.reset(),
  breadcrumbs: async () => {
    console.warn('coupon request is about to fail');
    await fetch('http://localhost:8098/api/coupon?token=never-recorded').catch(() => {});
  },
};

export default function App() {
  const [log, setLog] = useState<string[]>([]);
  const [dsn, setDsn] = useState('');
  const add = (line: string) => setLog((l) => [...l, `${new Date().toLocaleTimeString()}  ${line}`]);

  useEffect(() => {
    (async () => {
      const config = await loadConfig();
      if (!config) {
        add('Paste a DSN below and tap Save. Create one with: juju project create -org acme -name rn-example');
        return;
      }
      setDsn(config.dsn);
      await AsyncStorage.setItem(DSN_KEY, config.dsn);
      juju.init({ dsn: config.dsn, release: RELEASE, storage: AsyncStorage, debug: true, flushIntervalMs: 1000 });
      juju.setUser({ id: 'example-user' });
      await juju.ready();
      add(`juju initialised (scenario: ${config.scenario ?? 'none'})`);

      if (config.scenario === 'analytics') {
        // A known set, to check a chart against: 1 checkout_started, an
        // identify, then 1 more checkout_started after login.
        scenarios.checkout();
        scenarios.login();
        scenarios.checkout();
        await new Promise((r) => setTimeout(r, 100));
        await juju.flush();
        add('analytics scenario sent');
      } else if (config.scenario === 'nonfatal') {
        await scenarios.breadcrumbs();
        scenarios.captured();
        scenarios.rejection();
        add('non-fatal scenarios triggered');
      } else if (config.scenario === 'crash') {
        await scenarios.breadcrumbs();
        add('crashing in 1 s…');
        setTimeout(scenarios.crash, 1000);
      }
    })();
  }, []);

  return (
    <SafeAreaView style={styles.root}>
      <ScrollView contentContainerStyle={styles.body}>
        <Text style={styles.title}>juju React Native example</Text>
        <TextInput style={styles.input} value={dsn} onChangeText={setDsn} placeholder="http://pk_...@localhost:8080/1" autoCapitalize="none" autoCorrect={false} />
        <Button title="Save DSN" onPress={async () => (await AsyncStorage.setItem(DSN_KEY, dsn.trim()), add('saved; restart the app'))} />
        <View style={styles.buttons}>
          <Button title="Crash (fatal TypeError)" onPress={scenarios.crash} />
          <Button title="Unhandled promise rejection" onPress={scenarios.rejection} />
          <Button title="Handled error (captureException)" onPress={() => (scenarios.captured(), add('captured a SyntaxError'))} />
          <Button title="Breadcrumbs (console + fetch)" onPress={() => void scenarios.breadcrumbs().then(() => add('breadcrumbs recorded'))} />
          <Button title="Start checkout (capture)" onPress={() => (scenarios.checkout(), add('captured checkout_started'))} />
          <Button title="Log in as rn_user_1 (identify)" onPress={() => (scenarios.login(), add('identified as rn_user_1'))} />
          <Button title="Log out (reset)" onPress={() => (scenarios.logout(), add('reset: new anonymous id'))} />
          <Button title="Send now" onPress={() => void juju.flush().then(() => add('flushed'))} />
        </View>
        <Text style={styles.log} testID="log">{log.join('\n')}</Text>
      </ScrollView>
    </SafeAreaView>
  );
}

const styles = StyleSheet.create({
  root: { flex: 1, backgroundColor: '#0f1318' },
  body: { padding: 20, gap: 12 },
  title: { color: '#e6eaef', fontSize: 22, fontWeight: '700' },
  input: { borderWidth: 1, borderColor: '#2a333e', borderRadius: 6, padding: 10, color: '#e6eaef', fontFamily: 'Menlo' },
  buttons: { gap: 4 },
  log: { color: '#98a3b1', fontFamily: 'Menlo', fontSize: 12 },
});
