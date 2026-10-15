/**
 * uuidv7 mints a UUID version 7 identifier: a 48-bit big-endian millisecond
 * timestamp, the version nibble, the variant bits, and random fill for the
 * rest. The layout matches RFC 9562 and the server's own reader
 * (`uuidv7Millis` in `internal/api/handlers_pipelines_write.go`), which
 * decodes the first six bytes as the timestamp.
 *
 * The Run drawer (`pages/pipelines/RunDrawer.tsx`) uses this to mint one
 * client token per drawer open, so a repeated submit carries the same
 * token and the BFF answers with one run.
 */
export function uuidv7(): string {
  const bytes = new Uint8Array(16);
  const rnd = crypto.getRandomValues(new Uint8Array(10));

  const ts = Date.now();
  bytes[0] = (ts / 2 ** 40) & 0xff;
  bytes[1] = (ts / 2 ** 32) & 0xff;
  bytes[2] = (ts >>> 24) & 0xff;
  bytes[3] = (ts >>> 16) & 0xff;
  bytes[4] = (ts >>> 8) & 0xff;
  bytes[5] = ts & 0xff;

  bytes[6] = 0x70 | (rnd[0] & 0x0f); // version 7
  bytes[7] = rnd[1];
  bytes[8] = 0x80 | (rnd[2] & 0x3f); // variant 10
  bytes[9] = rnd[3];
  bytes[10] = rnd[4];
  bytes[11] = rnd[5];
  bytes[12] = rnd[6];
  bytes[13] = rnd[7];
  bytes[14] = rnd[8];
  bytes[15] = rnd[9];

  const hex = Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}
