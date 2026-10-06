// Canonical JSON for JavaScript values, mirroring internal/v8/canon.go byte for byte.
//
// Numbers use ECMAScript Number::toString. Strings escape exactly as
// JSON.stringify (well-formed: lone surrogates become lowercase \udxxx).
// Values JSON cannot express use single-key tagged objects:
//   undefined -> {"$undefined":true}     array hole  -> {"$hole":true}
//   Date      -> {"$date":"<toISOString() with trailing fractional zeros trimmed>"}; invalid Date -> {"$date":null}
//   BigInt    -> {"$bigint":"<dec>"}     non-finite and -0 -> {"$number":"NaN"|"Infinity"|"-Infinity"|"-0"}
//   bytes     -> {"$bytes":"<base64>"}   Map -> {"$map":[[k,v],...]}   Set -> {"$set":[...]}
//   RegExp    -> {"$regexp":[source,flags]}
//   Error     -> {"$error":{"name":n,"message":m,"stack":s?,"cause":c?}} (stack and cause only when present)
//   array with named (non-index) properties -> {"$array":[...],"$props":{...}}
//   wrapper   -> {"$wrapper":[kind,value]}
// A reference to an ancestor still being written (a cycle) is {"$cycle":<ancestor depth, root = 0>}.
// Shared, non-cyclic references are written out in full each time.

const str = (s) => JSON.stringify(s);

function dateString(d) {
  // toISOString is YYYY-MM-DDTHH:MM:SS.mmmZ, or +YYYYYY-/-YYYYYY- outside years 0000-9999.
  // Trailing fractional zeros are trimmed, and a zero fraction dropped, as Go's RFC3339Nano does.
  const iso = d.toISOString();
  const dot = iso.indexOf('.');
  const frac = iso.slice(dot + 1, dot + 4).replace(/0+$/, '');
  const base = iso.slice(0, dot);
  return frac === '' ? `${base}Z` : `${base}.${frac}Z`;
}

const isIndex = (k) => /^(0|[1-9][0-9]*)$/.test(k) && Number(k) <= 4294967294;

function number(n) {
  if (Number.isNaN(n)) return '{"$number":"NaN"}';
  if (n === Infinity) return '{"$number":"Infinity"}';
  if (n === -Infinity) return '{"$number":"-Infinity"}';
  if (Object.is(n, -0)) return '{"$number":"-0"}';
  return String(n);
}

function wrapperKind(v) {
  if (v instanceof Boolean) return 'Boolean';
  if (v instanceof Number) return 'Number';
  if (v instanceof String) return 'String';
  if (typeof v === 'object' && Object.prototype.toString.call(v) === '[object BigInt]') return 'BigInt';
  return null;
}

export function canonical(value) {
  const stack = [];
  const enc = (v) => {
    switch (typeof v) {
      case 'undefined': return '{"$undefined":true}';
      case 'boolean': return v ? 'true' : 'false';
      case 'number': return number(v);
      case 'bigint': return `{"$bigint":${str(v.toString())}}`;
      case 'string': return str(v);
      case 'object': break;
      default: throw new TypeError(`unsupported type ${typeof v}`);
    }
    if (v === null) return 'null';
    if (v instanceof Date) return Number.isNaN(v.getTime()) ? '{"$date":null}' : `{"$date":${str(dateString(v))}}`;
    if (v instanceof RegExp) return `{"$regexp":[${str(v.source)},${str(v.flags)}]}`;
    if (v instanceof ArrayBuffer) return `{"$bytes":"${Buffer.from(v).toString('base64')}"}`;
    if (ArrayBuffer.isView(v)) {
      return `{"$bytes":"${Buffer.from(v.buffer, v.byteOffset, v.byteLength).toString('base64')}"}`;
    }
    const kind = wrapperKind(v);
    if (kind !== null) return `{"$wrapper":[${str(kind)},${enc(v.valueOf())}]}`;

    const depth = stack.indexOf(v);
    if (depth >= 0) return `{"$cycle":${depth}}`;
    stack.push(v);
    try {
      if (v instanceof Error) {
        let out = `{"name":${str(v.name)},"message":${str(v.message)}`;
        if (typeof v.stack === 'string') out += `,"stack":${str(v.stack)}`;
        if (Object.hasOwn(v, 'cause')) out += `,"cause":${enc(v.cause)}`;
        return `{"$error":${out}}}`;
      }
      if (Array.isArray(v)) {
        const out = [];
        for (let i = 0; i < v.length; i++) out.push(i in v ? enc(v[i]) : '{"$hole":true}');
        const items = `[${out.join(',')}]`;
        const named = Object.keys(v).filter((k) => !isIndex(k));
        if (named.length === 0) return items;
        return `{"$array":${items},"$props":{${named.map((k) => `${str(k)}:${enc(v[k])}`).join(',')}}}`;
      }
      if (v instanceof Map) {
        return `{"$map":[${[...v].map(([k, x]) => `[${enc(k)},${enc(x)}]`).join(',')}]}`;
      }
      if (v instanceof Set) return `{"$set":[${[...v].map(enc).join(',')}]}`;
      return `{${Object.keys(v).map((k) => `${str(k)}:${enc(v[k])}`).join(',')}}`;
    } finally {
      stack.pop();
    }
  };
  return enc(value);
}
