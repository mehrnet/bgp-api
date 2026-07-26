import { describe, expect, test } from "bun:test";
import { parseIp, sortKeyToIp } from "../lib/ip";

describe("parseIp", () => {
  test("uses the producer's IPv4-mapped sort key", () => {
    expect(parseIp("1.1.1.1")).toEqual({
      canonical: "1.1.1.1",
      version: 4,
      sortKey: "000000000000000000000000281470698586369",
    });
  });

  test("normalizes and round-trips IPv6", () => {
    const parsed = parseIp("2001:0db8:0:0:0:0:0:1");
    expect(parsed).toEqual({ canonical: "2001:db8::1", version: 6, sortKey: "042540766411282592856903984951653826561" });
    expect(sortKeyToIp(parsed!.sortKey, 6)).toBe("2001:db8::1");
  });

  test("rejects invalid input", () => {
    expect(parseIp("1.1.1.999")).toBeNull();
    expect(parseIp("2001:::1")).toBeNull();
  });
});
