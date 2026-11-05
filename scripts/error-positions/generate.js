// Generates testdata/error_positions.json, the jsonata-js reference that
// TestErrorPositions compares gnata's error code, token and position with.
//
// Usage, from the repository root, with jsonata-js checked out at v2.2.2:
//
//	node scripts/error-positions/generate.js <jsonata-js dir> > testdata/error_positions.json
//
// The cases are every case of testdata/groups that jsonata-js fails (the
// timelimit cases aside), each again behind three prefixes holding Persian
// text, a zero-width non-joiner and an astral emoji, so that positions count
// UTF-16 code units; the extra cases below; and, as "mutation" cases, a
// deterministic sample of the syntax errors made by cutting each suite
// expression short or deleting one of its characters.
"use strict";
const fs = require("fs");
const path = require("path");

const jsDir = process.argv[2];
if (!jsDir) {
  console.error("usage: node scripts/error-positions/generate.js <jsonata-js dir>");
  process.exit(2);
}
const jsonata = require(path.resolve(jsDir, "src", "jsonata"));
const groupsDir = path.join("testdata", "groups");

const prefixes = ['("نام\u200cخانوادگی 🙂"; ', '($کالا\u200cها := "۱۲۳\u200c"; ', '(/* توضیح\u200c 😀 */ '];

const extra = [
  "result.(",
  "$value >",
  '$number("abc")',
  '"قیمت\u200cها" & $number("abc")',
  "`کالا\u200cها`.(",
  '"سلام\u200c" & (1 +',
  '($نرخ := function($x){$x * "الف"}; $نرخ(2))',
  '"ب\u200cج" & $substring(1)',
  '{"کلید\u200c": 1, "کلید\u200c": 2}',
  '"ی" > 1 and $number("x")',
  '"\u200c\u200c" & $f()',
  '"🙂🙂" & - "s"',
  '"آ" & $lookup(1,2,3,4)',
  '"🙂" & /*',
  '"ج\u200c" & "\\q"',
];

// Every mutationStride-th syntax error made from the suite is kept.
const mutationStride = 20;

async function run(tc) {
  let expr;
  try {
    expr = jsonata(tc.expr);
  } catch (e) {
    return e;
  }
  if (tc.mutation) {
    return null;
  }
  try {
    await expr.evaluate(tc.input, tc.bindings || {});
  } catch (e) {
    return e;
  }
  return null;
}

function suiteCases() {
  const cases = [];
  for (const group of fs.readdirSync(groupsDir).sort()) {
    const dir = path.join(groupsDir, group);
    if (!fs.statSync(dir).isDirectory()) continue;
    for (const file of fs.readdirSync(dir).filter((f) => f.endsWith(".json")).sort()) {
      const spec = JSON.parse(fs.readFileSync(path.join(dir, file), "utf8"));
      for (const tc of Array.isArray(spec) ? spec : [spec]) {
        if (tc.timelimit != null) continue;
        let expr = tc.expr;
        if (tc["expr-file"]) expr = fs.readFileSync(path.join(dir, tc["expr-file"]), "utf8");
        const c = { expr, bindings: tc.bindings || undefined };
        if ("data" in tc) {
          c.data = tc.data;
          c.input = tc.data;
        } else if (tc.dataset != null) {
          c.dataset = tc.dataset;
          c.input = JSON.parse(fs.readFileSync(path.join("testdata", "datasets", tc.dataset + ".json"), "utf8"));
        }
        cases.push(c);
      }
    }
  }
  return cases;
}

function mutations(exprs) {
  const out = new Set();
  for (const e of exprs) {
    if (e.length > 400) continue;
    for (let i = 1; i < e.length; i++) {
      out.add(e.slice(0, i));
      out.add(e.slice(0, i - 1) + e.slice(i));
    }
  }
  return [...out].sort();
}

function record(tc, e) {
  const r = { expr: tc.expr };
  if (tc.mutation) r.mutation = true;
  if (tc.dataset !== undefined) r.dataset = tc.dataset;
  if (tc.data !== undefined) r.data = tc.data;
  if (tc.bindings !== undefined) r.bindings = tc.bindings;
  r.code = e.code;
  if (e.token !== undefined && e.token !== null && String(e.token) !== "") r.token = String(e.token);
  if (typeof e.position === "number" && e.position !== 0) r.position = e.position;
  return r;
}

(async () => {
  const out = [];
  const suite = suiteCases();
  for (const tc of suite) {
    const e = await run(tc);
    if (!e || !e.code) continue;
    out.push(record(tc, e));
    for (const p of prefixes) {
      const v = { ...tc, expr: p + tc.expr + ")" };
      const ve = await run(v);
      if (ve && ve.code) out.push(record(v, ve));
    }
  }
  for (const expr of extra) {
    const tc = { expr, data: { x: 1 }, input: { x: 1 } };
    const e = await run(tc);
    if (e && e.code) out.push(record(tc, e));
  }
  let n = 0;
  for (const expr of mutations([...new Set(suite.map((c) => c.expr))])) {
    const tc = { expr, mutation: true };
    const e = await run(tc);
    if (!e || !e.code) continue;
    if (n++ % mutationStride === 0) out.push(record(tc, e));
  }
  process.stdout.write("[\n" + out.map((r) => JSON.stringify(r)).join(",\n") + "\n]\n");
  console.error("cases", out.length);
})();
