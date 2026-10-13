import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

import {
  MAX_JSON_DEPTH, decodeFirstJSON, goText, jsonValid, roomValueIsJSON, roomWireValue,
} from "../src/rooms/wire.js";

const shared = JSON.parse(readFileSync(new URL("../../testdata/room-wire-vectors.json", import.meta.url), "utf8"));

function bytesOf(v) {
  if (v.hex !== undefined) {
    return new Uint8Array(Buffer.from(v.hex, "hex"));
  }
  if (v.nest) {
    const { open, inner, close, depth } = v.nest;
    return new TextEncoder().encode(open.repeat(depth) + inner + close.repeat(depth));
  }
  return new TextEncoder().encode(v.in);
}

test("the shared vectors are read", () => {
  assert.ok(shared.vectors.length >= 40);
});

for (const v of shared.vectors) {
  test(`room wire vector: ${v.name}`, () => {
    const bytes = bytesOf(v);
    assert.equal(roomValueIsJSON(bytes), v.json);
    const wire = roomWireValue(bytes);
    if (v.json) {
      assert.equal(wire, v.wire ?? goText(bytes));
    } else {
      assert.equal(JSON.parse(wire), v.value ?? goText(bytes));
    }
  });
}

test("the nesting limit is Go's", () => {
  assert.equal(MAX_JSON_DEPTH, 10000);
  assert.ok(jsonValid(new TextEncoder().encode(`${"[".repeat(10000)}${"]".repeat(10000)}`)));
  assert.ok(!jsonValid(new TextEncoder().encode(`${"[{\"a\":".repeat(5001)}1${"}]".repeat(5001)}`)));
});

test("the first value of a body decodes and what follows is ignored", () => {
  const parse = (text) => decodeFirstJSON(new TextEncoder().encode(text));
  assert.deepEqual(parse('{"a":1} garbage'), { t: "object", entries: [["a", { t: "number", raw: "1" }]] });
  assert.deepEqual(parse("123abc"), { t: "number", raw: "123" });
  assert.equal(parse(""), null);
  assert.equal(parse("   "), null);
  assert.equal(parse('{"a":1'), null);
  assert.equal(parse('{"a" 1}'), null);
});

test("a decoded body keeps duplicate names in order and number literals", () => {
  const node = decodeFirstJSON(new TextEncoder().encode('{"a":1.0,"A":[1e0,null],"a":"x"}'));
  assert.deepEqual(node.entries.map(([key]) => key), ["a", "A", "a"]);
  assert.equal(node.entries[0][1].raw, "1.0");
  assert.deepEqual(node.entries[1][1].items, [{ t: "number", raw: "1e0" }, { t: "null" }]);
});

test("strings decode as Go unquotes them", () => {
  const string = (hex) => decodeFirstJSON(new Uint8Array(Buffer.from(hex, "hex"))).v;
  // "😀": an escaped pair joins into one character.
  assert.equal(string("225c75643833645c7564653030" + "22"), "\u{1f600}");
  // "\ud800x": a lone surrogate escape becomes U+FFFD.
  assert.equal(string("225c75643830307822"), "�x");
  // "\udc00\ud800": two lone halves, two replacements.
  assert.equal(string("225c75646330305c756438303022"), "��");
  // A truncated sequence costs one replacement per byte.
  assert.equal(string("22e28222"), "��");
});
