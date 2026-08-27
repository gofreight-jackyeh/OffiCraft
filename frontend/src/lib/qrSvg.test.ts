// qrSvg — the QR shown during TOTP enrolment.
//
// 🔴 WHAT THIS GUARDS. A wrong QR is not a visible bug: scanners accept it, the
// authenticator stores a wrong secret, and the owner only finds out when their
// codes are rejected at login — after the enrolment screen is gone. So the
// encoding itself is pinned against the SPEC's own structural invariants rather
// than against a screenshot, and the two properties that actually make a symbol
// scannable (quiet zone, opaque light background) are asserted directly, because
// both are silent when wrong: the SVG still renders, it just will not scan.

import { describe, it, expect } from "vitest";
import { qrSvg } from "./qrSvg";

/** A realistic payload — what the enrolment screen actually encodes. */
const OTPAUTH =
  "otpauth://totp/OffiCraft:owner?algorithm=SHA1&digits=6&issuer=OffiCraft" +
  "&period=30&secret=GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ";

/** Pull the module-grid span out of the viewBox. */
function span(svg: string): number {
  const m = svg.match(/viewBox="0 0 (\d+) \1"/);
  if (!m) throw new Error(`no square viewBox in: ${svg.slice(0, 120)}`);
  return Number(m[1]);
}

describe("qrSvg", () => {
  it("produces a self-contained square SVG with an accessible name", () => {
    const svg = qrSvg(OTPAUTH, { title: "設定金鑰" });
    expect(svg.startsWith("<svg")).toBe(true);
    expect(svg.endsWith("</svg>")).toBe(true);
    expect(svg).toContain('role="img"');
    expect(svg).toContain('aria-label="設定金鑰"');
    // Self-contained: nothing is FETCHED. The xmlns declaration is a required
    // namespace identifier, not a request, so it is excluded deliberately —
    // what must never appear is a remote asset, a data: URI or an <image>,
    // because a QR "service" would mean posting the owner's TOTP secret to a
    // third party.
    expect(svg).not.toContain("<image");
    expect(svg).not.toContain("data:");
    const urls = [...svg.matchAll(/https?:\/\/[^"']+/g)].map((m) => m[0]);
    expect(urls).toEqual(["http://www.w3.org/2000/svg"]);
  });

  // The quiet zone is 4 modules a side, per the spec. Scanners rely on it, and
  // a symbol flush to its container is the classic "why won't this scan".
  it("carries a 4-module quiet zone on every side", () => {
    const svg = qrSvg(OTPAUTH);
    const total = span(svg);
    // Every dark module sits at >= 4 and < total-4 in both axes.
    const coords = [...svg.matchAll(/M(\d+) (\d+)h1v1h-1z/g)].map((m) => [
      Number(m[1]),
      Number(m[2]),
    ]);
    expect(coords.length).toBeGreaterThan(100); // sanity: it really encoded something
    const min = Math.min(...coords.flat());
    const max = Math.max(...coords.flat());
    expect(min).toBe(4);
    expect(max).toBe(total - 5); // last module starts one before the quiet zone
  });

  // 🔴 An opaque light background, not transparency. On a dark theme a
  // transparent quiet zone lets the page bleed through and the scanner loses the
  // symbol's edge — the SVG looks fine and simply does not scan.
  it("paints an opaque light background rather than relying on the page", () => {
    const svg = qrSvg(OTPAUTH);
    expect(svg).toMatch(/<rect width="\d+" height="\d+" fill="#ffffff"\/>/);
    expect(svg).toContain('fill="#000000"');
  });

  // Structural invariants of a real QR symbol: the module count is always
  // 4*version+17, i.e. 21, 25, 29 … and always odd.
  it("emits a module count that is a legal QR version", () => {
    const modules = span(qrSvg(OTPAUTH)) - 8; // strip the quiet zone
    expect((modules - 17) % 4).toBe(0);
    expect(modules % 2).toBe(1);
    expect(modules).toBeGreaterThanOrEqual(21);
  });

  // Version auto-selection: more data must never produce a SMALLER symbol.
  it("grows the symbol as the payload grows", () => {
    const small = span(qrSvg("otpauth://totp/a:b?secret=AAAAAAAA"));
    const large = span(qrSvg(OTPAUTH + "&x=" + "y".repeat(300)));
    expect(large).toBeGreaterThan(small);
  });

  // Determinism — the same secret must render the same symbol every time, or an
  // owner re-reading the screen would be handed a different code to scan.
  it("is deterministic for a given payload", () => {
    expect(qrSvg(OTPAUTH)).toBe(qrSvg(OTPAUTH));
  });

  // Different secrets must not collide into the same picture.
  it("encodes different payloads differently", () => {
    const a = qrSvg(OTPAUTH);
    const b = qrSvg(OTPAUTH.replace("GEZDGNBV", "MZXW6YTB"));
    expect(a).not.toBe(b);
  });

  it("refuses an empty payload rather than rendering a blank symbol", () => {
    expect(() => qrSvg("")).toThrow(/empty/);
  });

  it("escapes the accessible name instead of injecting markup", () => {
    const svg = qrSvg(OTPAUTH, { title: '"><script>x</script>' });
    expect(svg).not.toContain("<script>");
    expect(svg).toContain("&quot;&gt;&lt;script&gt;");
  });
});
