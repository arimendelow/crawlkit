#!/usr/bin/env node
// Generates the V8 structured-clone test corpus under testdata/vectors.
//
//   node chromium/v8/testdata/gen/gen.mjs [--out <dir>]
//
// Output is deterministic: no timestamps, fixed values, sorted file names.
// Each vector NAME yields NAME.bin (wire version 15, Node 22's default) and
// NAME.v16.bin (the same bytes with the version byte rewritten to 16, as
// a downstream app's V16 rewrite does), each with a sibling .json holding the
// canonical JSON of the expected value. Hand-built vectors list their own
// versions (NAME.v13.bin etc.). Node writes typed-array views as host
// objects, so every view vector is built by hand. Error vectors go in
// errors/ without a .json.
//
// Regenerate by running this script; `go test ./chromium/v8` (TestGeneratorIsCurrent)
// fails on Node 22 when the committed files drift from this script's output on Node 22.
import fs from 'node:fs';
import path from 'node:path';
import v8 from 'node:v8';
import { fileURLToPath } from 'node:url';
import { canonical } from './canon.mjs';

const here = path.dirname(fileURLToPath(import.meta.url));
const outIdx = process.argv.indexOf('--out');
const outDir = path.resolve(outIdx > 0 ? process.argv[outIdx + 1] : path.join(here, '..', 'vectors'));

const files = new Map(); // relative name -> Buffer

function put(name, buf) {
  if (files.has(name)) throw new Error(`duplicate vector file ${name}`);
  files.set(name, Buffer.from(buf));
}

function suffix(version) {
  return version === 15 ? '' : `.v${version}`;
}

// --- Node-generated vectors -------------------------------------------------

function node(name, value, { roundTrip = true } = {}) {
  const bin = v8.serialize(value);
  if (bin[0] !== 0xff || bin[1] !== 15) throw new Error(`${name}: expected wire version 15, got ${bin[1]}`);
  const json = canonical(value);
  if (roundTrip) {
    const back = canonical(v8.deserialize(bin));
    if (back !== json) throw new Error(`${name}: Node round trip differs\n${json}\n${back}`);
  }
  put(`${name}.bin`, bin);
  put(`${name}.json`, json);
  const v16 = Buffer.from(bin);
  v16[1] = 16;
  put(`${name}.v16.bin`, v16);
  put(`${name}.v16.json`, json);
}

// --- hand-built vectors -----------------------------------------------------

const tag = (c) => c.charCodeAt(0);
const hdr = (version) => [0xff, version];

function varint(n) {
  let v = BigInt(n);
  const out = [];
  do {
    let b = Number(v & 0x7fn);
    v >>= 7n;
    if (v > 0n) b |= 0x80;
    out.push(b);
  } while (v > 0n);
  return out;
}

const zigzag = (n) => varint(n >= 0 ? n * 2 : -n * 2 - 1);
const latin1 = (s) => [...Buffer.from(s, 'latin1')];
const utf16 = (s) => [...Buffer.from(s, 'utf16le')];
const utf8 = (s) => [...Buffer.from(s, 'utf8')];

const B = {
  int32: (n) => [tag('I'), ...zigzag(n)],
  uint32: (n) => [tag('U'), ...varint(n)],
  double: (x) => {
    const b = Buffer.alloc(8);
    b.writeDoubleLE(x);
    return [tag('N'), ...b];
  },
  str1: (s) => [tag('"'), ...varint(s.length), ...latin1(s)],
  str2: (s) => [tag('c'), ...varint(s.length * 2), ...utf16(s)],
  str8: (s) => [tag('S'), ...varint(Buffer.byteLength(s)), ...utf8(s)],
  buffer: (bytes) => [tag('B'), ...varint(bytes.length), ...bytes],
  // View following a buffer (or a reference to one). flags only exist from version 14 (and in broken 13 data).
  view: (subtag, offset, length, version, forceFlags = false) => [
    tag('V'), tag(subtag), ...varint(offset), ...varint(length), ...(version >= 14 || forceFlags ? varint(0) : []),
  ],
  bigint: (neg, digitBytes) => [tag('Z'), ...varint((digitBytes.length << 1) | (neg ? 1 : 0)), ...digitBytes],
};

function digits64(v) {
  // little-endian 64-bit digits of a non-negative BigInt
  const out = [];
  do {
    out.push(...Buffer.from(BigInt.asUintN(64, v).toString(16).padStart(16, '0'), 'hex').reverse());
    v >>= 64n;
  } while (v > 0n);
  return out;
}

function hand(name, versions, build) {
  for (const version of versions) {
    const { bytes, json } = build(version);
    put(`${name}${suffix(version)}.bin`, Buffer.from(bytes));
    put(`${name}${suffix(version)}.json`, json);
  }
}

// --- the corpus -------------------------------------------------------------

// Oddballs
node('oddball_undefined', undefined);
node('oddball_null', null);
node('oddball_true', true);
node('oddball_false', false);

// Smi edge values (Node writes them as int32), int32 range, doubles
for (const [name, n] of [
  ['zero', 0], ['one', 1], ['minus_one', -1], ['smi_max', 2 ** 30 - 1], ['smi_min', -(2 ** 30)],
  ['smi_max_plus_1', 2 ** 30], ['smi_min_minus_1', -(2 ** 30) - 1],
  ['int32_max', 2 ** 31 - 1], ['int32_min', -(2 ** 31)], ['uint32_max', 2 ** 32 - 1],
  ['safe_max', Number.MAX_SAFE_INTEGER], ['two_53_plus_2', 2 ** 53 + 2],
  ['half', 0.5], ['neg_1_5', -1.5], ['tenth', 0.1], ['third', 1 / 3], ['pi', Math.PI],
  ['nan', NaN], ['inf', Infinity], ['neg_inf', -Infinity], ['neg_zero', -0],
  ['denorm_min', 5e-324], ['max_value', Number.MAX_VALUE], ['e21', 1e21], ['e_minus_7', 1e-7],
  ['big_round', 123456789012345680000], ['e20', 1e20], ['e_minus_6', 1e-6],
]) node(`number_${name}`, n);

// BigInt
for (const [name, n] of [
  ['zero', 0n], ['one', 1n], ['minus_one', -1n], ['two_64', 2n ** 64n], ['u64_max', 2n ** 64n - 1n],
  ['i64_min', -(2n ** 63n)], ['neg_two_100', -(2n ** 100n)], ['huge', 7n ** 200n],
]) node(`bigint_${name}`, n);

// Strings
for (const [name, s] of [
  ['empty', ''], ['ascii', 'hello world'], ['latin1', 'café ÿ\u0080'], ['cjk', '日本語'],
  ['emoji', 'a\u{1F600}\u{1F44D}\u{1F3FD}b'], ['rtl', 'שלום عالم مرحبا'],
  ['lone_high', '\ud800'], ['lone_low', 'a\udc00b'], ['lone_high_then_ascii', '\ud83dx'], ['swapped_pair', '\ude00\ud83d'],
  ['controls', 'a\u0000b\u0001\u001f\b\f\n\r\t\u007f'], ['quotes', 'say "hi" \\ back/slash'],
  ['line_sep', '  '], ['html', '<p>a &amp; b &lt; c</p>'], ['nul_only', '\u0000'],
  ['long_ascii', 'x'.repeat(300)], ['long_two_byte', '中'.repeat(200)],
]) node(`string_${name}`, s);

// Objects
node('object_empty', {});
node('object_simple', { a: 1, b: 'two', c: [3], d: null, e: true });
node('object_nested', { a: { b: { c: { d: [1, { e: 'f' }] } } } });
node('object_integer_keys', { b: 1, 2: 'two', 1: 'one', '-1': 'neg', '4294967295': 'notindex', '4294967294': 'maxindex', '01': 'lead', 1.5: 'frac' });
node('object_unicode_keys', { '日本': 1, 'café': 2, '\u{1F600}': 3, '': 4 });
node('object_undefined_values', { a: undefined, b: null });
node('object_with_date_and_bigint', { at: new Date(1e12), n: 12n });

// Arrays
node('array_empty', []);
node('array_dense', [1, 'two', 3.5, null, undefined, true]);
node('array_holes', [1, , 3, , ,]); // eslint-disable-line no-sparse-arrays
node('array_new_array_3', new Array(3));
node('array_undefined_not_holes', [undefined, undefined]);
node('array_nested', [[1, [2, [3, []]]], []]);
{
  const a = [1, 2];
  a.extra = 'kept';
  node('array_named_property', a);
}
{
  const a = [];
  a.name = 'empty but named';
  node('array_empty_named_property', a);
}
{
  const a = [1, , 3]; // eslint-disable-line no-sparse-arrays
  a.first = { n: 1 };
  a.second = [2];
  a[-1] = 'negative';
  a['4294967295'] = 'not an index';
  node('array_named_properties_many', a);
}
{
  const a = [1];
  a.self = a;
  node('array_named_property_cycle', a);
}
{
  const a = [];
  a.self = a;
  node('array_empty_cycle_through_property', a);
}
{
  const a = [];
  a[1030] = 'far';
  a[3] = 'near';
  node('array_sparse_dictionary', a);
}
{
  const a = [];
  a[5000] = { x: 1 };
  node('array_sparse_5000', a);
}

// Back-references and cycles
{
  const shared = { s: 1 };
  node('ref_same_object_twice', [shared, shared, { inner: shared }]);
}
{
  const a = { name: 'a' };
  a.self = a;
  node('ref_cycle_object', a);
}
{
  const arr = [1];
  arr.push(arr);
  node('ref_cycle_array', arr);
}
{
  const a = { name: 'a' };
  const b = { name: 'b', a };
  a.b = b;
  node('ref_cycle_mutual', { root: a, other: b });
}
{
  const m = new Map();
  m.set('me', m);
  node('ref_cycle_map', m);
}
{
  const s = new Set();
  s.add(s);
  node('ref_cycle_set', s);
}
{
  const d = new Date(86400000);
  const re = /shared/g;
  node('ref_shared_date_regexp', [d, d, re, re]);
}
{
  const str = 'strings are not objects';
  node('ref_strings_not_registered', [{ a: str }, str, { b: str }]);
}

// Dates
for (const [name, ms] of [
  ['epoch', 0], ['recent', Date.UTC(2024, 1, 29, 12, 34, 56, 789)], ['trailing_zero_ms', Date.UTC(2020, 0, 1, 0, 0, 0, 100)],
  ['ten_ms', Date.UTC(2020, 0, 1, 0, 0, 0, 10)], ['whole_second', Date.UTC(2021, 5, 7, 8, 9, 10, 0)],
  ['pre_epoch', -1], ['year_1', Date.UTC(1, 0, 1)], ['year_9999', Date.UTC(9999, 11, 31, 23, 59, 59, 999)],
  ['year_10000', Date.UTC(10000, 0, 1)], ['year_0', -62167219200000], ['year_minus_1', -62198755200000],
  ['max_js_date', 8.64e15], ['min_js_date', -8.64e15], ['invalid_nan', NaN],
]) {
  const d = new Date(ms);
  if (name === 'year_1') d.setUTCFullYear(1);
  node(`date_${name}`, d);
}

// RegExp
node('regexp_simple', /a+b/);
node('regexp_gi', /a+b/gi);
node('regexp_all_flags', /x(?<n>y)/dgimsuy);
node('regexp_v_flag', /[\p{L}--[a-z]]/v);
node('regexp_unicode_source', /café|日本/u);
node('regexp_empty', new RegExp(''));

// Errors (stack fixed so output is machine-independent)
function err(Ctor, message, opts) {
  const e = opts === undefined ? new Ctor(message) : new Ctor(message, opts);
  e.stack = `${e.name}: ${message}\n    at fixture (app.js:1:1)`;
  return e;
}
node('error_plain', err(Error, 'boom'));
node('error_type', err(TypeError, 'bad type'));
node('error_range', err(RangeError, 'out of range'));
node('error_eval', err(EvalError, 'eval'));
node('error_reference', err(ReferenceError, 'ref'));
node('error_syntax', err(SyntaxError, 'syntax'));
node('error_uri', err(URIError, 'uri'));
node('error_empty_message', err(Error, ''));
node('error_unicode_message', err(Error, 'café \u{1F600} 日'));
node('error_with_cause', err(Error, 'outer', { cause: { code: 7 } }));
node('error_with_error_cause', err(TypeError, 'outer', { cause: err(RangeError, 'inner') }));
node('error_with_null_cause', err(Error, 'outer', { cause: null }));
{
  const e = err(Error, 'self cause');
  e.cause = e;
  node('error_cause_cycle', e);
}
{
  const e = err(Error, 'no stack');
  delete e.stack;
  Object.defineProperty(e, 'stack', { value: undefined, configurable: true, writable: true });
  node('error_no_stack', e);
}
{
  const inner = err(Error, 'inner');
  node('error_in_object', { e: inner, again: inner });
}

// Wrappers
node('wrapper_true', new Boolean(true));
node('wrapper_false', new Boolean(false));
node('wrapper_number', new Number(1.5));
node('wrapper_number_int', new Number(42));
node('wrapper_number_nan', new Number(NaN));
node('wrapper_number_neg_zero', new Number(-0));
node('wrapper_bigint', Object(2n ** 70n));
node('wrapper_bigint_neg', Object(-5n));
node('wrapper_string', new String('héllo'));
node('wrapper_string_emoji', new String('\u{1F600}'));
node('wrapper_string_empty', new String(''));
{
  const w = new String('shared');
  node('wrapper_ref_twice', [w, w]);
}

// Map and Set
node('map_empty', new Map());
node('map_mixed_keys', new Map([['a', 1], [2, 'b'], [null, undefined], [true, false], [1.5, -0]]));
node('map_object_keys', new Map([[{ k: 1 }, [1]], [[2], { v: 2 }]]));
node('map_nested', new Map([['m', new Map([['x', new Set([1, 2])]])]]));
node('set_empty', new Set());
node('set_mixed', new Set([1, 'a', null, undefined, 2n, { o: 1 }]));
node('set_order', new Set([3, 1, 2, 'z', 'a']));

// ArrayBuffer (Node writes bare buffers natively)
node('arraybuffer_empty', new ArrayBuffer(0));
node('arraybuffer_bytes', new Uint8Array([0, 1, 2, 253, 254, 255]).buffer);
node('arraybuffer_large', new Uint8Array(300).map((_, i) => i & 0xff).buffer);
node('arraybuffer_resizable', new ArrayBuffer(2, { maxByteLength: 8 }), { roundTrip: false });
node('arraybuffer_in_object', { data: new Uint8Array([9, 8, 7]).buffer, n: 1 });
{
  const ab = new Uint8Array([1, 2, 3]).buffer;
  node('arraybuffer_shared_ref', [ab, ab]);
}

// A chat-thread-shaped message chain (synthetic: no real data)
{
  const t0 = Date.UTC(2026, 8, 30, 10, 15, 30, 123);
  const msg = (n, from, content, extra = {}) => ({
    id: String(1700000000000 + n),
    type: 'Message',
    messagetype: 'RichText/Html',
    contenttype: 'Text',
    content,
    from: `https://chat.example.test/v1/users/ME/contacts/8:orgid:${from}`,
    imdisplayname: from === 'u1' ? 'Ada Lovelace' : 'שלום \u{1F600}',
    originalarrivaltime: new Date(t0 + n * 1000).toISOString(),
    composetime: new Date(t0 + n * 1000),
    version: BigInt(1700000000000 + n),
    clientmessageid: String(9000000000000 + n),
    properties: { emotions: [], mentions: n % 2 ? [{ id: 0, mri: '8:orgid:u2' }] : [], files: '[]', importance: '' },
    ...extra,
  });
  const chain = {
    id: '19:abc@thread.tacv2;messageid=1700000000001',
    conversationId: '19:abc@thread.tacv2',
    replyChainId: '1700000000001',
    tenantId: '00000000-0000-0000-0000-000000000001',
    creator: '8:orgid:u1',
    latestDeliveryTime: new Date(t0 + 5000),
    isRead: false,
    unreadCount: 2,
    messageMap: new Map([
      ['1700000000001', msg(1, 'u1', '<p>Hello &amp; welcome \u{1F44B}</p>')],
      ['1700000000002', msg(2, 'u2', '<div>שלום</div><img src="x">', { messagetype: 'RichText/Html', deleted: undefined })],
      ['1700000000003', msg(3, 'u1', '', { messagetype: 'ThreadActivity/AddMember', content: '<addmember></addmember>' })],
    ]),
    messages: [msg(1, 'u1', 'one'), msg(2, 'u2', 'two'), msg(3, 'u1', 'three')],
    cards: [{ type: 'AdaptiveCard', body: [{ type: 'TextBlock', text: 'café' }], fallback: null }],
    sparseMeta: Object.assign([], { 0: 'a', 7: 'h' }),
    seen: new Set(['u1', 'u2']),
  };
  chain.messages[2].replyTo = chain.messages[0]; // shared reference
  chain.self = chain; // cycle
  node('message_chain', chain);
}

// --- hand-built: tags Node cannot emit or that vary by wire version ----------

hand('number_utag_small', [15, 16], (v) => ({ bytes: [...hdr(v), ...B.uint32(5)], json: '5' }));
hand('number_utag_multibyte', [15, 16], (v) => ({ bytes: [...hdr(v), ...B.uint32(300)], json: '300' }));
hand('number_utag_max', [15, 16], (v) => ({ bytes: [...hdr(v), ...B.uint32(0xffffffff)], json: '4294967295' }));
hand('number_int32_zigzag_min', [15], (v) => ({ bytes: [...hdr(v), ...B.int32(-(2 ** 31))], json: '-2147483648' }));
hand('number_double_integral', [15], (v) => ({ bytes: [...hdr(v), ...B.double(4)], json: '4' }));
hand('number_double_neg_zero', [15], (v) => ({ bytes: [...hdr(v), ...B.double(-0)], json: '{"$number":"-0"}' }));
hand('bigint_zero_digits', [15], (v) => ({ bytes: [...hdr(v), ...B.bigint(false, [])], json: '{"$bigint":"0"}' }));
hand('bigint_two_64_handbuilt', [15], (v) => ({ bytes: [...hdr(v), ...B.bigint(false, digits64(2n ** 64n))], json: '{"$bigint":"18446744073709551616"}' }));

hand('misc_padding_between_tokens', [15], (v) => ({
  bytes: [...hdr(v), 0, 0, tag('o'), 0, ...B.str1('a'), 0, ...B.int32(1), tag('{'), 1],
  json: '{"a":1}',
}));
hand('misc_verify_object_count', [15], (v) => ({
  bytes: [...hdr(v), tag('?'), 2, tag('o'), ...B.str1('k'), tag('?'), 3, ...B.int32(1), tag('{'), 1],
  json: '{"k":1}',
}));
hand('misc_trailing_padding', [15], (v) => ({ bytes: [...hdr(v), ...B.int32(3), 0, 0, 0], json: '3' }));
hand('object_uint32_and_double_keys', [15], (v) => ({
  bytes: [...hdr(v), tag('o'), ...B.uint32(7), ...B.str1('u'), ...B.double(1.5), ...B.str1('d'), ...B.int32(-2), ...B.str1('n'), tag('{'), 3],
  json: '{"7":"u","1.5":"d","-2":"n"}',
}));
hand('object_duplicate_key_last_wins', [15], (v) => ({
  bytes: [...hdr(v), tag('o'), ...B.str1('a'), ...B.int32(1), ...B.str1('b'), ...B.int32(2), ...B.str1('a'), ...B.int32(3), tag('{'), 3],
  json: '{"a":3,"b":2}',
}));

hand('array_sparse_handbuilt_with_named_property', [15, 16], (v) => ({
  bytes: [...hdr(v), tag('a'), 10, ...B.int32(2), ...B.int32(5), ...B.int32(7), ...B.str1('x'), ...B.str1('name'), ...B.str1('kept'), tag('@'), 3, 10],
  json: '{"$array":[{"$hole":true},{"$hole":true},5,{"$hole":true},{"$hole":true},{"$hole":true},{"$hole":true},"x",{"$hole":true},{"$hole":true}],"$props":{"name":"kept"}}',
}));
hand('array_dense_the_hole_tag', [15, 16], (v) => ({
  bytes: [...hdr(v), tag('A'), 3, tag('-'), ...B.int32(1), tag('-'), tag('$'), 0, 3],
  json: '[{"$hole":true},1,{"$hole":true}]',
}));
hand('array_dense_trailing_index_property', [15], (v) => ({
  bytes: [...hdr(v), tag('A'), 2, ...B.int32(1), ...B.int32(2), ...B.int32(1), ...B.str1('patched'), tag('$'), 1, 2],
  json: '[1,"patched"]',
}));
hand('array_dense_undefined_not_hole', [13, 14], (v) => ({
  bytes: [...hdr(v), tag('A'), 2, tag('_'), ...B.int32(1), tag('$'), 0, 2],
  json: '[{"$undefined":true},1]',
}));

hand('regexp_string_value', [13, 14], (v) => ({
  bytes: [...hdr(v), tag('R'), ...B.str1('ab'), 1],
  json: '{"$regexp":["ab","g"]}',
}));
hand('string_utf8_tag', [13, 14, 15, 16], (v) => ({
  bytes: [...hdr(v), ...B.str8('café \u{1F600}')],
  json: JSON.stringify('café \u{1F600}'),
}));
hand('string_utf8_invalid_bytes', [15], (v) => ({
  bytes: [...hdr(v), tag('S'), 3, 0x61, 0xff, 0x62],
  json: '"a�b"',
}));
hand('string_two_byte_with_padding', [15, 16], (v) => ({
  bytes: [...hdr(v), 0, ...B.str2('日本')],
  json: '"日本"',
}));

// Array buffer views. Node serializes these as host objects, so the bytes are built by hand.
const seq = (n) => Array.from({ length: n }, (_, i) => (i * 7 + 3) & 0xff);
const b64 = (bytes) => Buffer.from(bytes).toString('base64');
const bytesJSON = (bytes) => `{"$bytes":"${b64(bytes)}"}`;

hand('view_uint8', [13, 14, 15, 16], (v) => {
  const data = seq(10);
  return { bytes: [...hdr(v), ...B.buffer(data), ...B.view('B', 2, 5, v)], json: bytesJSON(data.slice(2, 7)) };
});
hand('view_uint8_multibyte_lengths', [14, 15, 16], (v) => {
  const data = seq(300);
  return { bytes: [...hdr(v), ...B.buffer(data), ...B.view('B', 130, 170, v)], json: bytesJSON(data.slice(130, 300)) };
});
hand('view_dataview', [13, 15, 16], (v) => {
  const data = seq(16);
  return { bytes: [...hdr(v), ...B.buffer(data), ...B.view('?', 1, 9, v)], json: bytesJSON(data.slice(1, 10)) };
});
hand('view_float64', [15, 16], (v) => {
  const data = seq(32);
  return { bytes: [...hdr(v), ...B.buffer(data), ...B.view('F', 8, 16, v)], json: bytesJSON(data.slice(8, 24)) };
});
hand('view_int16_uint32_bigint64', [15], (v) => {
  const data = seq(32);
  return {
    bytes: [...hdr(v), tag('A'), 3, ...B.buffer(data), ...B.view('w', 2, 6, v), ...B.buffer(data), ...B.view('D', 4, 8, v), ...B.buffer(data), ...B.view('q', 8, 24, v), tag('$'), 0, 3],
    json: `[${bytesJSON(data.slice(2, 8))},${bytesJSON(data.slice(4, 12))},${bytesJSON(data.slice(8, 32))}]`,
  };
});
hand('view_empty', [15], (v) => ({ bytes: [...hdr(v), ...B.buffer([]), ...B.view('B', 0, 0, v)], json: bytesJSON([]) }));
// Version 13 data written by the buggy Chromium encoder includes the flags field.
hand('view_v13_broken_flags', [13], (v) => {
  const data = seq(8);
  return { bytes: [...hdr(v), ...B.buffer(data), ...B.view('B', 1, 4, v, true)], json: bytesJSON(data.slice(1, 5)) };
});
hand('view_after_buffer_reference', [15, 16], (v) => {
  const data = seq(12);
  // ids: 0 array, 1 buffer, 2 first view, 3 second view
  return {
    bytes: [...hdr(v), tag('A'), 3, ...B.buffer(data), ...B.view('B', 0, 4, v), tag('^'), 1, ...B.view('B', 8, 4, v), tag('^'), 1, tag('$'), 0, 3],
    json: `[${bytesJSON(data.slice(0, 4))},${bytesJSON(data.slice(8, 12))},${bytesJSON(data)}]`,
  };
});
hand('arraybuffer_resizable_handbuilt', [16], (v) => ({
  bytes: [...hdr(v), tag('~'), 2, 8, 0x0a, 0x0b], json: bytesJSON([0x0a, 0x0b]),
}));
hand('arraybuffer_immutable', [16], (v) => ({
  bytes: [...hdr(v), tag('C'), 3, 1, 2, 3], json: bytesJSON([1, 2, 3]),
}));

// Errors with sub-tags in other orders, and old V8's missing pieces
hand('error_message_only_handbuilt', [15], (v) => ({
  bytes: [...hdr(v), tag('r'), tag('T'), tag('m'), ...B.str1('x'), tag('.')],
  json: '{"$error":{"name":"TypeError","message":"x"}}',
}));
hand('error_bare_end', [15], (v) => ({ bytes: [...hdr(v), tag('r'), tag('.')], json: '{"$error":{"name":"Error","message":""}}' }));
hand('error_cause_then_stack', [15], (v) => ({
  bytes: [...hdr(v), tag('r'), tag('m'), ...B.str1('m'), tag('c'), ...B.int32(1), tag('s'), ...B.str1('st'), tag('.')],
  json: '{"$error":{"name":"Error","message":"m","stack":"st","cause":1}}',
}));

// --- error inputs (no .json) -------------------------------------------------

put('errors/host_buffer.bin', v8.serialize(Buffer.from('host')));
put('errors/host_uint8array.bin', v8.serialize(new Uint8Array([1, 2, 3])));

// --- write --------------------------------------------------------------------

fs.mkdirSync(path.join(outDir, 'errors'), { recursive: true });
for (const dir of [outDir, path.join(outDir, 'errors')]) {
  for (const f of fs.readdirSync(dir)) {
    if (/\.(bin|json)$/.test(f)) fs.rmSync(path.join(dir, f));
  }
}
for (const name of [...files.keys()].sort()) {
  fs.writeFileSync(path.join(outDir, name), files.get(name));
}
console.log(`wrote ${files.size} files to ${outDir}`);
