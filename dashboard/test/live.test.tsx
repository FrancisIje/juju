import { renderHook } from "@testing-library/react";
import { useLive } from "../src/lib/live";

// A stand-in for the browser's EventSource that the test drives by hand.
class FakeEventSource {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 2;
  static all: FakeEventSource[] = [];
  readyState = FakeEventSource.CONNECTING;
  onopen: (() => void) | null = null;
  onmessage: (() => void) | null = null;
  onerror: (() => void) | null = null;
  closed = false;
  constructor(readonly url: string) {
    FakeEventSource.all.push(this);
  }
  open() {
    this.readyState = FakeEventSource.OPEN;
    this.onopen?.();
  }
  message() {
    this.onmessage?.();
  }
  fail(permanently: boolean) {
    this.readyState = permanently ? FakeEventSource.CLOSED : FakeEventSource.CONNECTING;
    this.onerror?.();
  }
  close() {
    this.closed = true;
    this.readyState = FakeEventSource.CLOSED;
  }
}

let visibility: DocumentVisibilityState = "visible";
function setVisibility(v: DocumentVisibilityState) {
  visibility = v;
  document.dispatchEvent(new Event("visibilitychange"));
}

beforeEach(() => {
  FakeEventSource.all = [];
  visibility = "visible";
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.spyOn(document, "visibilityState", "get").mockImplementation(() => visibility);
});

test("refreshes on each message, and once after a reconnect", () => {
  const refresh = vi.fn();
  renderHook(() => useLive(3, refresh));
  const es = FakeEventSource.all[0]!;
  expect(es.url).toBe("/api/ui/projects/3/live");

  es.open();
  expect(refresh).not.toHaveBeenCalled(); // the page just loaded its data
  es.message();
  es.message();
  expect(refresh).toHaveBeenCalledTimes(2);

  // A dropped connection: the browser reconnects by itself, and anything
  // that changed meanwhile was missed, so refresh once.
  es.fail(false);
  es.open();
  expect(refresh).toHaveBeenCalledTimes(3);
});

test("a hidden tab lets go of its connection, and catches up when shown", () => {
  const refresh = vi.fn();
  renderHook(() => useLive(3, refresh));
  const first = FakeEventSource.all[0]!;
  first.open();

  setVisibility("hidden");
  expect(first.closed).toBe(true);

  setVisibility("visible");
  expect(refresh).toHaveBeenCalledTimes(1);
  expect(FakeEventSource.all).toHaveLength(2);
  expect(FakeEventSource.all[1]!.closed).toBe(false);
});

test("falls back to polling when the stream is refused", () => {
  vi.useFakeTimers();
  try {
    const refresh = vi.fn();
    renderHook(() => useLive(3, refresh));
    FakeEventSource.all[0]!.fail(true); // e.g. the server answered 404
    vi.advanceTimersByTime(30_000);
    expect(refresh).toHaveBeenCalledTimes(1);
    vi.advanceTimersByTime(30_000);
    expect(refresh).toHaveBeenCalledTimes(2);
  } finally {
    vi.useRealTimers();
  }
});

test("unmounting or switching project closes the stream", () => {
  const { rerender, unmount } = renderHook(({ id }) => useLive(id, () => {}), { initialProps: { id: 3 } });
  rerender({ id: 4 });
  expect(FakeEventSource.all[0]!.closed).toBe(true);
  expect(FakeEventSource.all[1]!.url).toBe("/api/ui/projects/4/live");
  unmount();
  expect(FakeEventSource.all[1]!.closed).toBe(true);
});
