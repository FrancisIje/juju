import { describe, expect, it } from "vitest";
import { DsnError, parseDsn } from "../src/dsn";

describe("parseDsn", () => {
  it("splits a DSN into what the transport needs", () => {
    expect(parseDsn("https://pk_abc@juju.example.com/42")).toEqual({
      publicKey: "pk_abc",
      origin: "https://juju.example.com",
      projectId: "42",
      endpoint: "https://juju.example.com/api/v1/projects/42/events",
    });
  });

  it("allows http and a port for local development", () => {
    expect(parseDsn("http://pk_abc@localhost:8080/1").endpoint).toBe("http://localhost:8080/api/v1/projects/1/events");
  });

  it("doesn't use the URL class, which is incomplete in React Native", () => {
    const realURL = globalThis.URL;
    // @ts-expect-error simulating React Native, where URL getters throw
    globalThis.URL = class {
      constructor() {
        throw new Error("URL.username is not implemented");
      }
    };
    try {
      expect(parseDsn("https://pk_abc@juju.example.com/1").publicKey).toBe("pk_abc");
    } finally {
      globalThis.URL = realURL;
    }
  });

  it("lowercases the host and allows a trailing slash", () => {
    expect(parseDsn("https://pk_abc@Juju.Example.com/9/").endpoint).toBe("https://juju.example.com/api/v1/projects/9/events");
  });

  it.each([
    ["not a url", "pk_abc"],
    ["no key", "https://juju.example.com/1"],
    ["no project id", "https://pk_abc@juju.example.com/"],
    ["non-numeric project", "https://pk_abc@juju.example.com/shop"],
    ["wrong scheme", "ftp://pk_abc@juju.example.com/1"],
  ])("rejects %s with a helpful message", (_name, dsn) => {
    expect(() => parseDsn(dsn)).toThrow(DsnError);
  });
});
