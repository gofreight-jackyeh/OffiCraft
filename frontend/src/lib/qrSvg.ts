// qrSvg.ts — turn a string into an inline SVG QR code.
//
// WHY A LIBRARY HERE, when this repo hand-rolled TOTP rather than add one: the
// two are not the same size of problem. RFC 6238 is an HMAC, a truncation and a
// modulo — forty lines. A QR encoder is Reed–Solomon over GF(256), matrix
// placement, eight mask patterns with penalty scoring, and format/version bits;
// a correct one is several hundred lines, and getting it subtly wrong produces a
// symbol that scanners happily accept and then decode to the WRONG secret. That
// failure is invisible until an owner cannot log in. `qrcode-generator` is MIT,
// has NO dependencies of its own, and ships types.
//
// WHAT IS NOT DELEGATED: the rendering. The library can emit an <img> tag or
// draw to a canvas; both are declined. We read the module boundary out of it and
// build the SVG ourselves, so:
//   * no canvas, no data: URI, no image decoding on the path that displays a
//     credential;
//   * the markup is ours, so it inherits currentColor and the page's theme
//     rather than baking a colour;
//   * nothing ever leaves the browser. A QR "service" (chart.googleapis.com and
//     friends) would mean posting the owner's TOTP secret to a third party.
//     There is no such call anywhere in this repo and there must not be.

import qrcode from "qrcode-generator";

/** Error-correction level. "M" (~15% recovery) is the usual choice for an
 * otpauth URI: "L" is fragile against a slightly dirty screen, and "Q"/"H" grow
 * the module count enough to make the symbol denser than a phone camera enjoys
 * at the size this renders. */
const EC_LEVEL = "M" as const;

/** Quiet zone in MODULES. The spec says 4 and scanners genuinely rely on it —
 * a symbol rendered flush to its container is the classic "why won't it scan". */
const QUIET_ZONE = 4;

export interface QrSvgOptions {
  /** Rendered edge length in CSS pixels. */
  size?: number;
  /** Accessible name for the <svg>. */
  title?: string;
}

/**
 * Render `text` as a self-contained SVG string.
 *
 * Type version 0 = "pick the smallest that fits", so an otpauth URI of any
 * reasonable length works without the caller doing capacity arithmetic.
 *
 * The dark modules are emitted as ONE `<path>` rather than a rect per module:
 * a version-6 symbol is ~1,800 modules, and 1,800 elements is a real cost in
 * both bytes and layout. `shape-rendering="crispEdges"` keeps the module edges
 * from being antialiased into grey, which is what makes a scaled-down QR fail
 * to scan.
 */
export function qrSvg(text: string, opts: QrSvgOptions = {}): string {
  const { size = 200, title = "QR code" } = opts;
  if (!text) throw new Error("qrSvg: refusing to encode an empty string");

  const qr = qrcode(0, EC_LEVEL);
  qr.addData(text);
  qr.make();

  const count = qr.getModuleCount();
  const span = count + QUIET_ZONE * 2;

  let path = "";
  for (let row = 0; row < count; row++) {
    for (let col = 0; col < count; col++) {
      if (qr.isDark(row, col)) {
        path += `M${col + QUIET_ZONE} ${row + QUIET_ZONE}h1v1h-1z`;
      }
    }
  }

  // viewBox is in MODULE units, so the symbol scales to any pixel size without
  // re-encoding and without fractional module boundaries.
  return [
    `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${span} ${span}"`,
    ` width="${size}" height="${size}" role="img" aria-label="${escapeAttr(title)}"`,
    ` shape-rendering="crispEdges">`,
    // The quiet zone must be LIGHT, not transparent: on a dark theme a
    // transparent margin lets the page's background bleed in and the scanner
    // loses the symbol's edge.
    `<rect width="${span}" height="${span}" fill="#ffffff"/>`,
    `<path d="${path}" fill="#000000"/>`,
    `</svg>`,
  ].join("");
}

/** Minimal attribute escaping for the one interpolated attribute above. */
function escapeAttr(s: string): string {
  return s
    .replace(/&/g, "&amp;")
    .replace(/"/g, "&quot;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;");
}
