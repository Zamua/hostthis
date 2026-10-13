// HTTP dates as hostthisd writes and reads them: Go's http.TimeFormat out, and
// http.ParseTime in, which tries TimeFormat, RFC 850 and ANSI C asctime with
// time.Parse's rules. Pure, no I/O.

// Go's http.TimeFormat, "Mon, 02 Jan 2006 15:04:05 GMT", for a time in ms.
export function formatHTTPDate(ms) {
  return new Date(ms).toUTCString();
}

const SHORT_DAYS = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];
const LONG_DAYS = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"];
const SHORT_MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];

// Each layout as Go's nextStdChunk splits it: a literal prefix, then a field.
const LAYOUTS = [
  // http.TimeFormat
  [["", "weekday"], [", ", "zeroDay"], [" ", "month"], [" ", "longYear"], [" ", "hour"],
    [":", "zeroMinute"], [":", "zeroSecond"], [" GMT", null]],
  // time.RFC850
  [["", "longWeekday"], [", ", "zeroDay"], ["-", "month"], ["-", "year"], [" ", "hour"],
    [":", "zeroMinute"], [":", "zeroSecond"], [" ", "zone"], ["", null]],
  // time.ANSIC
  [["", "weekday"], [" ", "month"], [" ", "underDay"], [" ", "hour"], [":", "zeroMinute"],
    [":", "zeroSecond"], [" ", "longYear"], ["", null]],
];

class Bad extends Error {}

const isDigit = (s, i) => i < s.length && s[i] >= "0" && s[i] <= "9";

// Go's skip: a space in the prefix matches one or more in the value.
function skip(value, prefix) {
  while (prefix.length) {
    if (prefix[0] === " ") {
      if (value.length && value[0] !== " ") throw new Bad();
      prefix = prefix.replace(/^ +/, "");
      value = value.replace(/^ +/, "");
      continue;
    }
    if (!value.length || value[0] !== prefix[0]) throw new Bad();
    prefix = prefix.slice(1);
    value = value.slice(1);
  }
  return value;
}

// Go's lookup: ASCII letters compare without case.
function lookup(table, value) {
  for (let i = 0; i < table.length; i++) {
    const want = table[i];
    if (value.length < want.length) continue;
    let ok = true;
    for (let j = 0; j < want.length && ok; j++) {
      const a = value.charCodeAt(j);
      const b = want.charCodeAt(j);
      ok = a === b || ((a | 0x20) === (b | 0x20) && (a | 0x20) >= 0x61 && (a | 0x20) <= 0x7a);
    }
    if (ok) return [i, value.slice(want.length)];
  }
  throw new Bad();
}

// Go's getnum: one or two digits, exactly two when fixed.
function getnum(value, fixed) {
  if (!isDigit(value, 0)) throw new Bad();
  if (!isDigit(value, 1)) {
    if (fixed) throw new Bad();
    return [Number(value[0]), value.slice(1)];
  }
  return [Number(value.slice(0, 2)), value.slice(2)];
}

// Go's atoi: an optional sign, then digits only.
function atoi(text) {
  const m = /^([+-]?)(\d*)$/.exec(text);
  if (!m) throw new Bad();
  const n = m[2] === "" ? 0 : Number(m[2]);
  return m[1] === "-" ? -n : n;
}

// Go's parseSignedOffset: the length of a "+h" or "-hh" offset up to 23, or 0.
function signedOffset(value) {
  const m = /^[+-](\d+)/.exec(value);
  return m && Number(m[1]) <= 23 ? m[0].length : 0;
}

// Go's parseTimeZone: the length of a zone name at the start of value, or 0.
function zoneLength(value) {
  if (value.length < 3) return 0;
  if (value.startsWith("ChST") || value.startsWith("MeST")) return 4;
  if (value.startsWith("GMT")) return 3 + (value.length > 3 ? signedOffset(value.slice(3)) : 0);
  if (value[0] === "+" || value[0] === "-") return signedOffset(value);
  let upper = 0;
  while (upper < 6 && upper < value.length && value[upper] >= "A" && value[upper] <= "Z") upper++;
  if (upper === 3) return 3;
  if (upper === 4 && (value[3] === "T" || value.startsWith("WITA"))) return 4;
  if (upper === 5 && value[4] === "T") return 5;
  return 0;
}

function daysIn(month, year) {
  if (month === 2) return year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0) ? 29 : 28;
  return [4, 6, 9, 11].includes(month) ? 30 : 31;
}

// Days from 1970-01-01 to the proleptic Gregorian date.
function daysFromCivil(year, month, day) {
  const y = month <= 2 ? year - 1 : year;
  const era = Math.floor(y / 400);
  const yoe = y - era * 400;
  const doy = Math.floor((153 * (month + (month > 2 ? -3 : 9)) + 2) / 5) + day - 1;
  const doe = yoe * 365 + Math.floor(yoe / 4) - Math.floor(yoe / 100) + doy;
  return era * 146097 + doe - 719468;
}

// One layout; the time in whole Unix seconds, or a Bad throw. A zone name
// leaves the instant as written: hostthisd runs in UTC, where Go's Parse knows
// no zone abbreviation but UTC and records the others with a zero offset.
function parseLayout(layout, value) {
  let year = 0;
  let month = 1;
  let day = 1;
  let hour = 0;
  let min = 0;
  let sec = 0;
  for (const [prefix, field] of layout) {
    value = skip(value, prefix);
    switch (field) {
      case null:
        if (value.length) throw new Bad();
        break;
      case "weekday":
        value = lookup(SHORT_DAYS, value)[1];
        break;
      case "longWeekday":
        value = lookup(LONG_DAYS, value)[1];
        break;
      case "month":
        [month, value] = lookup(SHORT_MONTHS, value);
        month++;
        break;
      case "zeroDay":
        [day, value] = getnum(value, true);
        break;
      case "underDay":
        if (value[0] === " ") value = value.slice(1);
        [day, value] = getnum(value, false);
        break;
      case "longYear":
        if (value.length < 4 || !isDigit(value, 0)) throw new Bad();
        year = atoi(value.slice(0, 4));
        value = value.slice(4);
        break;
      case "year":
        if (value.length < 2) throw new Bad();
        year = atoi(value.slice(0, 2));
        year += year >= 69 ? 1900 : 2000;
        value = value.slice(2);
        break;
      case "hour":
        [hour, value] = getnum(value, false);
        if (hour >= 24) throw new Bad();
        break;
      case "zeroMinute":
        [min, value] = getnum(value, true);
        if (min >= 60) throw new Bad();
        break;
      case "zeroSecond":
        [sec, value] = getnum(value, true);
        if (sec >= 60) throw new Bad();
        // A fraction the layout does not name is accepted and dropped.
        if (value.length >= 2 && (value[0] === "." || value[0] === ",") && isDigit(value, 1)) {
          let n = 2;
          while (isDigit(value, n)) n++;
          value = value.slice(n);
        }
        break;
      case "zone":
        if (value.startsWith("UTC")) {
          value = value.slice(3);
        } else {
          const n = zoneLength(value);
          if (!n) throw new Bad();
          value = value.slice(n);
        }
        break;
      default:
        throw new Error(`unknown layout field ${field}`);
    }
  }
  if (day < 1 || day > daysIn(month, year)) throw new Bad();
  return daysFromCivil(year, month, day) * 86400 + hour * 3600 + min * 60 + sec;
}

// Go's http.ParseTime, as whole Unix seconds (its fraction cannot change how
// it compares with a time truncated to the second), or null.
export function parseHTTPDate(text) {
  for (const layout of LAYOUTS) {
    try {
      return parseLayout(layout, text);
    } catch (error) {
      if (!(error instanceof Bad)) throw error;
    }
  }
  return null;
}
