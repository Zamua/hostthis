import assert from "node:assert/strict";
import test from "node:test";

import { etagMatches } from "../src/public/conditional.js";
import { formatHTTPDate, parseHTTPDate } from "../src/public/httpdate.js";
import { queryFlags } from "../src/public/request.js";

const T = Date.UTC(2026, 5, 7, 14) / 1000;

test("dates format as http.TimeFormat", () => {
  assert.equal(formatHTTPDate(T * 1000 + 999), "Sun, 07 Jun 2026 14:00:00 GMT");
  assert.equal(formatHTTPDate(0), "Thu, 01 Jan 1970 00:00:00 GMT");
});

// Each case is what Go's http.ParseTime returns for the text, by its layouts'
// rules, in a process whose local zone is UTC.
test("dates parse as http.ParseTime does", () => {
  const cases = [
    ["Sun, 07 Jun 2026 14:00:00 GMT", T],
    ["sun, 07 JUN 2026 14:00:00 GMT", T],
    ["Mon, 07 Jun 2026 14:00:00 GMT", T],
    ["Sun,   07 Jun 2026 14:00:00 GMT", T],
    ["Sun, 07 Jun 2026 4:00:00 GMT", T - 10 * 3600],
    ["Sun, 07 Jun 2026 14:00:00.123456789123 GMT", T],
    ["Sun, 07 Jun 2026 14:00:00,5 GMT", T],
    ["Sun, 29 Feb 2024 00:00:00 GMT", Date.UTC(2024, 1, 29) / 1000],
    ["Sunday, 07-Jun-26 14:00:00 GMT", T],
    ["Sunday, 07-Jun-26 14:00:00 GMT+3", T],
    ["Sunday, 07-Jun-26 14:00:00 EST", T],
    ["Sunday, 07-Jun-26 14:00:00 UTC", T],
    ["Sunday, 07-Jun-26 14:00:00 +05", T],
    ["Sunday, 07-Jun-26 14:00:00 ChST", T],
    ["Sunday, 07-Jun-69 14:00:00 GMT", Date.UTC(1969, 5, 7, 14) / 1000],
    ["Sunday, 07-Jun-68 14:00:00 GMT", Date.UTC(2068, 5, 7, 14) / 1000],
    ["Sunday, 07-Jun-+5 14:00:00 GMT", Date.UTC(2005, 5, 7, 14) / 1000],
    ["Sun Jun  7 14:00:00 2026", T],
    ["Sun Jun 7 14:00:00 2026", T],
    ["Sun Jun 07 14:00:00 2026", T],
    ["Sun, 07 Jun 2026 14:00:00 gmt", null],
    ["Sun, 07 Jun 2026 14:00:00 UTC", null],
    ["Sun, 7 Jun 2026 14:00:00 GMT", null],
    ["Sun, 07 Jun 26 14:00:00 GMT", null],
    ["Sun, 07 Jun 2026 24:00:00 GMT", null],
    ["Sun, 07 Jun 2026 14:60:00 GMT", null],
    ["Sun, 29 Feb 2025 00:00:00 GMT", null],
    ["Sun, 31 Jun 2026 00:00:00 GMT", null],
    ["Sun, 07 Jun 2026 14:00:00 GMTx", null],
    ["Sunday, 07-Jun-26 14:00:00 Est", null],
    ["Sunday, 07-Jun-26 14:00:00 UTCX", null],
    ["Sunday, 07-Jun-26 14:00:00 GMT+24", null],
    ["Sun, 07 Jun 2026", null],
    ["", null],
    ["yesterday", null],
  ];
  for (const [text, want] of cases) assert.equal(parseHTTPDate(text), want, JSON.stringify(text));
});

test("If-None-Match compares weakly, and * matches anything", () => {
  assert.ok(etagMatches("\"a\"", "\"a\""));
  assert.ok(etagMatches("W/\"a\"", "\"a\""));
  assert.ok(etagMatches("\"a\"", "W/\"a\""));
  assert.ok(etagMatches("\"b\" ,\t\"a\"", "\"a\""));
  assert.ok(etagMatches(" * ", "\"a\""));
  assert.ok(!etagMatches("\"b\", *", "\"a\""));
  assert.ok(!etagMatches("a", "\"a\""));
  assert.ok(!etagMatches("", "\"a\""));
  assert.ok(!etagMatches(null, "\"a\""));
});

test("query flags read as Go's URL.Query().Has", () => {
  assert.deepEqual(queryFlags("raw&files=0"), { raw: true, files: true });
  assert.deepEqual(queryFlags("r%61w=%41"), { raw: true, files: false });
  assert.deepEqual(queryFlags("raw;x=1&files"), { raw: false, files: true });
  assert.deepEqual(queryFlags("raw=%g0"), { raw: false, files: false });
  assert.deepEqual(queryFlags("raw+=1"), { raw: false, files: false });
  assert.deepEqual(queryFlags("&&raw&&"), { raw: true, files: false });
  assert.deepEqual(queryFlags(`${"a&".repeat(9999)}raw`), { raw: true, files: false });
  assert.deepEqual(queryFlags(`${"a&".repeat(10000)}raw`), { raw: false, files: false });
});
