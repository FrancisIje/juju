import { describe, expect, it } from "vitest";
import { exceptionFromThrown, parseStack } from "../src/stacktrace";

// Real traces copied from each browser, for the same bug.
const chrome = `TypeError: Cannot read properties of undefined (reading 'price')
    at lineTotal (https://shop.example.com/assets/index.4b8c2d1e.js:1:48213)
    at Array.map (<anonymous>)
    at async CartSummary (https://shop.example.com/assets/index.4b8c2d1e.js:1:51022)
    at https://shop.example.com/node_modules/react-dom/index.js:12:300`;

const firefox = `lineTotal@https://shop.example.com/assets/index.4b8c2d1e.js:1:48213
CartSummary@https://shop.example.com/assets/index.4b8c2d1e.js:1:51022
@https://shop.example.com/assets/index.4b8c2d1e.js:1:100`;

describe("parseStack", () => {
  it("parses Chrome / V8 traces, crash site first", () => {
    expect(parseStack(chrome)).toEqual([
      { function: "lineTotal", filename: "https://shop.example.com/assets/index.4b8c2d1e.js", lineno: 1, colno: 48213, in_app: true },
      { function: "Array.map", filename: "<anonymous>", in_app: false },
      { function: "CartSummary", filename: "https://shop.example.com/assets/index.4b8c2d1e.js", lineno: 1, colno: 51022, in_app: true },
      { filename: "https://shop.example.com/node_modules/react-dom/index.js", lineno: 12, colno: 300, in_app: false },
    ]);
  });

  it("parses Firefox / Safari traces", () => {
    const frames = parseStack(firefox);
    expect(frames).toHaveLength(3);
    expect(frames[0]).toEqual({ function: "lineTotal", filename: "https://shop.example.com/assets/index.4b8c2d1e.js", lineno: 1, colno: 48213, in_app: true });
    expect(frames[2]).not.toHaveProperty("function"); // anonymous function
  });

  it("gives the same function and file in both browsers, so fingerprints match", () => {
    const pick = (s: string) => parseStack(s).filter((f) => f.in_app).map((f) => [f.function, f.filename]);
    expect(pick(firefox).slice(0, 2)).toEqual(pick(chrome));
  });

  it("uses the isInApp function it is given", () => {
    const frames = parseStack(chrome, (f) => f.includes("react-dom"));
    expect(frames.map((f) => f.in_app)).toEqual([false, false, false, true]);
  });

  it("skips lines that are not frames", () => {
    expect(parseStack("Error: contact user@example.com\n    at f (a.js:1:2)")).toHaveLength(1);
    expect(parseStack(undefined)).toEqual([]);
  });
});

describe("Hermes (React Native) traces", () => {
  it("parses release frames, which say 'address at'", () => {
    const hermes = `TypeError: Cannot read property 'amount' of undefined
    at checkout (address at index.android.bundle:1:234567)
    at onPress (address at index.android.bundle:1:234890)
    at anonymous (native)`;
    expect(parseStack(hermes)).toEqual([
      { function: "checkout", filename: "index.android.bundle", lineno: 1, colno: 234567, in_app: true },
      { function: "onPress", filename: "index.android.bundle", lineno: 1, colno: 234890, in_app: true },
      { function: "anonymous", filename: "native", in_app: false },
    ]);
  });

  it("parses debug frames served by Metro", () => {
    const [f] = parseStack("    at checkout (http://10.0.2.2:8081/index.bundle//&platform=android&dev=true:123:45)");
    expect(f).toMatchObject({ function: "checkout", filename: "http://10.0.2.2:8081/index.bundle//&platform=android&dev=true", lineno: 123, colno: 45 });
  });
});

describe("exceptionFromThrown", () => {
  it("uses the error's name, message and stack", () => {
    class PaymentError extends Error {
      override name = "PaymentError";
    }
    const ex = exceptionFromThrown(new PaymentError("card declined"));
    expect(ex.type).toBe("PaymentError");
    expect(ex.message).toBe("card declined");
    expect(ex.stacktrace?.frames.length).toBeGreaterThan(0);
  });

  it("handles thrown strings and plain objects", () => {
    expect(exceptionFromThrown("oops")).toEqual({ type: "Error", message: "oops" });
    expect(exceptionFromThrown({ code: 42 })).toEqual({ type: "Error", message: 'Non-Error value thrown: {"code":42}' });
    expect(exceptionFromThrown(undefined).message).toBe("Non-Error value thrown: undefined");
  });

  it("handles circular objects without throwing", () => {
    const a: Record<string, unknown> = {};
    a.self = a;
    expect(exceptionFromThrown(a).type).toBe("Error");
  });

  it("recognises errors from another realm (iframe) by shape", () => {
    const foreign = { name: "TypeError", message: "x is undefined", stack: "TypeError: x\n    at f (https://a.com/a.js:1:1)" };
    expect(exceptionFromThrown(foreign).type).toBe("TypeError");
  });
});
