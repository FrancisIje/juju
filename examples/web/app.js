// The example app. In a real app this is the only juju code you'd write:
// juju.init(...) once at startup.

const params = new URLSearchParams(location.search);
const log = (msg) => {
  document.getElementById("log").textContent += `${new Date().toLocaleTimeString()}  ${msg}\n`;
};

// The DSN comes from ?dsn=... (used by automated runs) or what you saved.
let dsn = params.get("dsn") || "";
try {
  dsn = dsn || localStorage.getItem("juju-example-dsn") || "";
} catch {
  // storage can be blocked; the URL parameter still works
}
document.getElementById("dsn").value = dsn;

document.getElementById("save-dsn").addEventListener("click", () => {
  try {
    localStorage.setItem("juju-example-dsn", document.getElementById("dsn").value.trim());
  } catch {}
  location.search = "";
});

if (!dsn) {
  log("Paste a DSN above and save it. Create one with: go run ./cmd/juju project create -org acme -name web-example");
} else {
  juju.init({
    dsn,
    release: "example@0.1.0",
    environment: "development",
    debug: true,
    flushIntervalMs: 1000, // short, so you see results quickly; the default is 5 s
  });
  juju.setUser({ id: "example-user" });
  log(`juju initialised for ${dsn.replace(/\/\/[^@]+@/, "//pk_…@")}`);
}

function checkout(cart) {
  // A realistic bug: the coupon API failed, so cart.coupon is undefined.
  return cart.items.reduce((sum, item) => sum + item.price, 0) - cart.coupon.amount;
}

const actions = {
  "throw-uncaught": () => {
    log("throwing an uncaught TypeError…");
    // setTimeout so the error escapes this click handler and hits window.onerror
    setTimeout(() => checkout({ items: [{ price: 10 }] }));
  },
  "throw-rejection": () => {
    log("rejecting a promise nobody catches…");
    Promise.reject(new Error("Checkout API failed: 503"));
  },
  "throw-captured": () => {
    try {
      JSON.parse("{not json");
    } catch (err) {
      const id = juju.captureException(err, { tags: { step: "parse-config" } });
      log(`captured a handled SyntaxError, event_id ${id}`);
    }
  },
  "failed-fetch": async () => {
    const res = await fetch("/api/coupon?token=never-recorded");
    console.warn("coupon request returned", res.status);
    log(`fetch /api/coupon → ${res.status} (recorded as a breadcrumb, query string removed)`);
  },
  navigate: () => {
    history.pushState(null, "", "/cart" + location.search);
    log("navigated to /cart (recorded as a breadcrumb)");
  },
  flush: async () => {
    await juju.flush();
    log("flushed");
  },
};
for (const [id, fn] of Object.entries(actions)) document.getElementById(id).addEventListener("click", fn);

// The app's own error handler. It must keep working with juju installed.
window.onerror = (message) => {
  log(`app's own window.onerror still ran: ${message}`);
};

// #selftest runs every scenario on load, for automated checks with a
// headless browser. Clicking the buttons does the same thing by hand.
if (location.hash === "#selftest" && dsn) {
  (async () => {
    document.getElementById("navigate").click();
    await actions["failed-fetch"]();
    document.getElementById("throw-captured").click();
    document.getElementById("throw-rejection").click();
    document.getElementById("throw-uncaught").click();
    log("selftest: all scenarios triggered");
  })();
}
