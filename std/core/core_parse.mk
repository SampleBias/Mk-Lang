// Why: reading numbers from text must accept exactly the strings Go's strconv
// accepts and give the correctly rounded value, at run time and at compile
// time, without allocating.
//
// What: str.ParseInt, str.ParseFloat, str.ParseFloatPrefix, and the
// multiprecision Decimal (Read, shifts, rounding) that float parsing and
// core_fmtfloat.mk's long fixed output share.
//
// How: a port of Go 1.27's atof.go, uscale.go and decimal.go: Clinger's exact
// fast path, then one fast unrounded scaling (ParseScaled64), and the exact
// Decimal only when the scaling cannot decide the rounding. Decimal holds up
// to 800 ASCII digits in a fixed array, so even the slow path allocates
// nothing. Up to 19 digit bytes, which fit a u64, both parsers read the
// digits with nothing counted per byte; ParseInt's digits and ParseFloat's
// fraction digits go eight at a time where a whole word is digits
// (IsDigits8, Digits8); longer texts, which may still hold
// a small value behind leading zeros, take a digit-by-digit reader
// (ParseIntLong, LongDigits).

import std.raw

/// Parses the text as a decimal i64. ParseError when the text is not one or the
/// value does not fit. O(n); allocates nothing.
///
/// The text is an optional + or -, then one or more ASCII decimal digits,
/// and nothing else: no whitespace, no _ and no prefix such as 0x. The value
/// must fit in an i64.
export str.ParseInt(self): ParseError!i64 {
    b := self.AsBytes()
    n := b.Len()
    var i := 0
    var neg := false
    if n > 0 && (b[0] == 0x2B || b[0] == 0x2D) {
        neg = b[0] == 0x2D
        i = 1
    }
    if i == n {
        fail ParseError
    }
    if n - i > 19 {
        return try ParseIntLong(b, i, neg)
    }
    // The magnitude in u64; the limit is 2^63 for a negative value, else
    // 2^63 - 1. At most 19 digits are below 10^19 < 2^64, so the digits
    // never wrap and one test against the limit at the end is enough.
    limit: u64 = if neg { 0x8000000000000000 } else { 0x7FFFFFFFFFFFFFFF }
    var v: u64 = 0
    for n - i >= 8 {
        w := LoadWord(b, i)
        if !IsDigits8(w) {
            fail ParseError
        }
        v = v *% 100000000 +% Digits8(w)
        i += 8
    }
    for i < n {
        d := b[i] -% 0x30
        if d > 9 {
            fail ParseError
        }
        v = v *% 10 +% u64(d)
        i += 1
    }
    if v > limit {
        fail ParseError
    }
    if neg {
        return Trunc[i64](~v +% 1)
    }
    return Trunc[i64](v)
}

// ParseIntLong is str.ParseInt for more than 19 bytes after the sign,
// from b[start..]: leading zeros may still leave a value that fits, so
// each digit is tested against the limit.
ParseIntLong(b: []u8, start: int, neg: bool): ParseError!i64 {
    n := b.Len()
    var i := start
    limit: u64 = if neg { 0x8000000000000000 } else { 0x7FFFFFFFFFFFFFFF }
    var v: u64 = 0
    for i < n {
        c := b[i]
        if c < 0x30 || c > 0x39 {
            fail ParseError
        }
        d := u64(c - 0x30)
        if v > (limit - d) / 10 {
            fail ParseError
        }
        v = v * 10 + d
        i += 1
    }
    if neg {
        return Trunc[i64](~v +% 1)
    }
    return Trunc[i64](v)
}

/// Parses the text as an f64, correctly rounded. ParseError when the text is
/// not a number or the value overflows. O(n) for up to 19 significant digits,
/// more for the multiprecision fallback; allocates nothing.
///
/// The text is an optional + or -, then digits, digits., digits.digits or
/// .digits, then an optional exponent (e or E, an optional sign, digits).
/// It may instead be inf, infinity or nan in any case, the first two with
/// an optional sign. Nothing else parses: no whitespace, no _ and no hex. A
/// value too small for an f64 is a zero with the text's sign; a value too
/// large is ParseError.
export str.ParseFloat(self): ParseError!f64 {
    b := self.AsBytes()
    n := b.Len()
    var i := 0
    var neg := false
    if n > 0 && (b[0] == 0x2B || b[0] == 0x2D) {
        neg = b[0] == 0x2D
        i = 1
    }
    if i < n && (b[i] | 0x20) >= 0x61 {
        return try ParseFloatWord(b, i, neg)
    }
    // The digits, then at most one point and more digits, with nothing
    // counted per byte: up to 19 digit bytes, leading zeros included, fit
    // mant exactly, and the value is mant * 10^(dp - nd_mant) with dp the
    // negated count of fraction digits and nd_mant 0. Past 19, mant wraps
    // here and LongDigits reads the same bytes again with truncation.
    start := i
    var mant: u64 = 0
    for i < n {
        d := b[i] -% 0x30
        if d > 9 {
            break
        }
        mant = mant *% 10 +% u64(d)
        i += 1
    }
    var nfrac := 0
    var point := 0
    if i < n && b[i] == 0x2E {
        i += 1
        point = 1
        fstart := i
        for n - i >= 8 {
            w := LoadWord(b, i)
            if !IsDigits8(w) {
                break
            }
            mant = mant *% 100000000 +% Digits8(w)
            i += 8
        }
        for i < n {
            d := b[i] -% 0x30
            if d > 9 {
                break
            }
            mant = mant *% 10 +% u64(d)
            i += 1
        }
        nfrac = i - fstart
    }
    ndigits := i - start - point
    if ndigits == 0 {
        fail ParseError
    }
    var dp := -nfrac
    var nd_mant := 0
    var trunc := false
    if ndigits > 19 {
        ld := LongDigits(b, start, i)
        mant = ld.mant
        dp = ld.dp
        nd_mant = ld.nd_mant
        trunc = ld.trunc
    }
    if i < n && (b[i] | 0x20) == 0x65 {
        i += 1
        var esign := 1
        if i < n && (b[i] == 0x2B || b[i] == 0x2D) {
            if b[i] == 0x2D {
                esign = -1
            }
            i += 1
        }
        if i >= n || b[i] < 0x30 || b[i] > 0x39 {
            fail ParseError
        }
        // The exponent saturates at ExpCap, far beyond any text length, so
        // the leading zeros or the digits it offsets are always counted
        // exactly (Go 1.27 stops at 10000, so it reads
        // "0.<100000 zeros>1e100005" as 0, not 1e5).
        var e := 0
        for i < n && b[i] >= 0x30 && b[i] <= 0x39 {
            if e < ExpCap {
                e = e * 10 + Trunc[int](b[i] - 0x30)
            }
            i += 1
        }
        dp += e * esign
    }
    if i != n {
        fail ParseError
    }
    sign: u64 = if neg { 0x8000000000000000 } else { 0 }
    if mant == 0 {
        return raw.F64From(sign)
    }
    p := dp - nd_mant
    if p > 310 {
        fail ParseError // at least 10^291: past the largest f64
    }
    if p < -345 {
        return raw.F64From(sign) // below 10^-326: rounds to zero
    }
    if !trunc && mant >> 52 == 0 {
        // Clinger: an exact mantissa times an exact power of ten rounds once.
        pow := &F64Pow10
        var f := Cast[f64](mant)
        if neg {
            f = -f
        }
        if p == 0 {
            return f
        }
        if p > 0 && p <= 37 {
            var q := p
            if q > 22 {
                f = f * pow[q - 22]
                q = 22
            }
            if f <= 1e15 && f >= -1e15 {
                return f * pow[q]
            }
        } else if p < 0 && p >= -22 {
            return f / pow[-p]
        }
    }
    if trunc {
        return try ParseFloatTrunc(b, mant, p, sign)
    }
    r := ParseScaled64(mant, p, sign)
    if r.over {
        fail ParseError
    }
    return raw.F64From(r.bits)
}

// ParseFloatTrunc is ParseFloat's value for a text b with more than 19
// significant digits: mant is the first 19 and nonzero digits were cut
// after them. When mant and mant + 1 scale to the same f64, so does every
// value between; otherwise the exact Decimal decides.
ParseFloatTrunc(b: []u8, mant: u64, p: int, sign: u64): ParseError!f64 {
    r := ParseScaled64(mant, p, sign)
    r1 := ParseScaled64(mant + 1, p, sign)
    if r1.bits == r.bits {
        if r.over {
            fail ParseError
        }
        return raw.F64From(r.bits)
    }
    var d := Decimal.Zero()
    d.Read(b)
    db := d.FloatBits64()
    if db.over {
        fail ParseError
    }
    return raw.F64From(db.bits)
}

// The digits of a float's text with more than 19 of them, read as the
// first 19 significant digits (mant, nd_mant of them), the place of the
// point (the value is mant * 10^(dp - nd_mant)) and whether a nonzero
// digit was cut after them.
struct ParsedDigits {
    mant: u64
    nd_mant: int
    dp: int
    trunc: bool
}

// LongDigits reads b[start..end], which ParseFloat has checked: digits
// with at most one point. Leading zeros move the point and are not
// counted as significant.
LongDigits(b: []u8, start: int, end: int): ParsedDigits {
    var mant: u64 = 0
    var nd := 0
    var nd_mant := 0
    var dp := 0
    var saw_dot := false
    var trunc := false
    for i in start..end {
        c := b[i]
        if c == 0x2E {
            saw_dot = true
            dp = nd
        } else if c == 0x30 && nd == 0 {
            dp -= 1 // leading zeros
        } else {
            nd += 1
            if nd_mant < 19 {
                mant = mant *% 10 +% u64(c - 0x30) // below 10^19: no wrap
                nd_mant += 1
            } else if c != 0x30 {
                trunc = true
            }
        }
    }
    if !saw_dot {
        dp = nd
    }
    return ParsedDigits(mant: mant, nd_mant: nd_mant, dp: dp, trunc: trunc)
}

// IsDigits8 reports whether each byte of w is an ASCII digit: adding 0x46
// sets the top bit of a byte from 0x3A to 0xB9, subtracting 0x30 that of a
// byte below 0x30 or above 0xB9, and neither carries into the next byte
// while the lower ones are digits.
IsDigits8(w: u64): bool {
    return ((w +% 0x4646464646464646) | (w -% 0x3030303030303030)) & 0x8080808080808080 == 0
}

// Digits8 is the value of the eight ASCII digits in w, the first in the
// low byte (LoadWord's little-endian order), by pairs, then fours, then
// all eight.
Digits8(w: u64): u64 {
    var v := w -% 0x3030303030303030
    v = (v *% 10) +% (v >> 8)
    lo := (v & 0x000000FF000000FF) *% 0x000F424000000064
    hi := ((v >> 16) & 0x000000FF000000FF) *% 0x0000271000000001
    return (lo +% hi) >> 32
}

/// Parses the longest prefix of the text that is a float by
/// str.ParseFloat's grammar, and returns its value and how many bytes it
/// used. The value is ParseFloat's, correctly rounded, except that a value
/// too large is an infinity with the text's sign where ParseFloat fails; a
/// value too small is a zero with the text's sign. When no prefix is a
/// float, used is 0 and the value is NaN. A written inf and a value too
/// large both give an infinity; the prefix's text, self[0..used], tells
/// them apart. No whitespace is skipped. O(n) for up to 19 significant
/// digits, more for the multiprecision fallback; allocates nothing.
///
/// The prefix stops before the first byte that cannot extend it: "1.5e" and
/// "1.5e+" use 3 ("1.5"), "5." uses 2, "." and "+" use 0, "1_000" uses 1,
/// "infinit" uses 3 ("inf"), and "-nan" uses 0 (nan takes no sign).
export str.ParseFloatPrefix(self): (value: f64, used: int) {
    b := self.AsBytes()
    used := FloatPrefixLen(b)
    if used == 0 {
        return (value: raw.F64From(0x7FF8000000000000), used: 0)
    }
    // The prefix is in the grammar, so ParseFloat fails only on overflow.
    // It ends after an ASCII byte, so it is on a scalar boundary.
    v := self[0..used].ParseFloat() else {
        if b[0] == 0x2D {
            return (value: raw.F64From(0xFFF0000000000000), used: used)
        }
        return (value: raw.F64From(0x7FF0000000000000), used: used)
    }
    return (value: v, used: used)
}

// FloatPrefixLen is the length of the longest prefix of b that ParseFloat's
// grammar accepts, 0 when none: one pass, each byte read once.
FloatPrefixLen(b: []u8): int {
    n := b.Len()
    var i := 0
    if n > 0 && (b[0] == 0x2B || b[0] == 0x2D) {
        i = 1
    }
    if i < n && (b[i] | 0x20) >= 0x61 {
        rest := n - i
        if rest >= 8 && LowerIs(b, i, "infinity") {
            return i + 8
        }
        if rest >= 3 && LowerIs(b, i, "inf") {
            return i + 3
        }
        if i == 0 && rest >= 3 && LowerIs(b, 0, "nan") {
            return 3
        }
        return 0
    }
    start := i
    for i < n && b[i] >= 0x30 && b[i] <= 0x39 {
        i += 1
    }
    whole := i > start
    if i < n && b[i] == 0x2E {
        var k := i + 1
        for k < n && b[k] >= 0x30 && b[k] <= 0x39 {
            k += 1
        }
        if !whole && k == i + 1 {
            return 0 // "." or a sign then "."
        }
        i = k
    } else if !whole {
        return 0
    }
    if i < n && (b[i] | 0x20) == 0x65 {
        var k := i + 1
        if k < n && (b[k] == 0x2B || b[k] == 0x2D) {
            k += 1
        }
        if k < n && b[k] >= 0x30 && b[k] <= 0x39 {
            for k < n && b[k] >= 0x30 && b[k] <= 0x39 {
                k += 1
            }
            i = k
        }
    }
    return i
}

// ParseFloatWord parses inf, infinity (after an optional sign) or nan (with
// none) in any case from b[i..].
ParseFloatWord(b: []u8, i: int, neg: bool): ParseError!f64 {
    rest := b.Len() - i
    if rest == 3 && LowerIs(b, i, "inf") || rest == 8 && LowerIs(b, i, "infinity") {
        if neg {
            return raw.F64From(0xFFF0000000000000)
        }
        return raw.F64From(0x7FF0000000000000)
    }
    if i == 0 && rest == 3 && LowerIs(b, 0, "nan") {
        return raw.F64From(0x7FF8000000000000) // the canonical NaN (types.CanonicalNaN64)
    }
    fail ParseError
}

// LowerIs reports whether b[i..] starts with the lowercase ASCII word w,
// ignoring the case of letters in b.
LowerIs(b: []u8, i: int, w: str): bool {
    for k in 0..w.Len() {
        if b[i + k] | 0x20 != w.Byte(k) {
            return false
        }
    }
    return true
}

// ExpCap is where an exponent's value stops growing: 2^48, past the length
// of any text, so e * 10 + 9 and the point's offset stay far inside int.
const ExpCap: int = 0x1000000000000

const F64Pow10: [23]f64 = [1e0, 1e1, 1e2, 1e3, 1e4, 1e5, 1e6, 1e7, 1e8, 1e9, 1e10, 1e11, 1e12, 1e13,
    1e14, 1e15, 1e16, 1e17, 1e18, 1e19, 1e20, 1e21, 1e22]

// FloatBits is an f64's bits and whether the value rounded past the
// largest finite f64 (bits then hold the infinity).
struct FloatBits {
    bits: u64
    over: bool
}

// ParseScaled64 is parseFloat64: d * 10^p (d >= 1, at most 19 digits,
// -345 <= p <= 310) rounded to nearest-even, with sign's bit 63.
ParseScaled64(d: u64, p: int, sign: u64): FloatBits {
    b := BitLen64(d)
    lp := FpLog2Pow10(p)
    var e := IntMin(1074, 53 - b - lp)
    pre := FpPrescale(e - (64 - b), p, lp)
    if pre.s >= 64 {
        return FloatBits(bits: sign, over: false)
    }
    var u := FpUscale(d << (64 - b), pre)
    if u >= FpUnmin(0x20000000000000) {
        u = (u >> 1) | (u & 1)
        e -= 1
    }
    m := sign | FpRound(u)
    if m & 0x10000000000000 == 0 {
        return FloatBits(bits: m, over: false) // subnormal or zero
    }
    if -e >= 0x7FF - 1075 {
        return FloatBits(bits: (m & 0x8000000000000000) | 0x7FF0000000000000, over: true)
    }
    return FloatBits(bits: (m & ~Trunc[u64](0x10000000000000)) | (Trunc[u64](1075 - e) << 52), over: false)
}

// A multiprecision decimal (Go's decimal.go): the value is 0.d[0..nd] *
// 10^dp, the digits ASCII, at most 800 of them (enough for every f64 the
// shifts below produce); trunc records nonzero digits dropped past 800.
// It is the slow path of parsing (more than 19 significant digits, when the
// fast path cannot decide) and of fixed formatting past 18 digits.
struct Decimal {
    d: [800]u8
    nd: int
    dp: int
    neg: bool
    trunc: bool
}

Decimal.Zero(): Decimal {
    return Decimal(d: [0; 800], nd: 0, dp: 0, neg: false, trunc: false)
}

// Read fills the decimal from b, which ParseFloat has checked against the
// grammar: a sign, digits with at most one point, then an exponent. The
// point is placed by every significant digit read, kept or not: Go 1.27's
// decimal.set places it by the kept ones, so a text with more than 800
// significant digits before the point is read too small
// ("9007199254740993", 900 zeros, "1e-901" is 9.007e-102 in Go 1.27, and
// 9007199254740994 correctly rounded).
Decimal.Read(mut self, b: []u8) {
    n := b.Len()
    var i := 0
    if b[0] == 0x2B || b[0] == 0x2D {
        self.neg = b[0] == 0x2D
        i = 1
    }
    var saw_dot := false
    var all := 0
    for i < n {
        c := b[i]
        if c == 0x2E {
            saw_dot = true
            self.dp = all
        } else if c >= 0x30 && c <= 0x39 {
            if c == 0x30 && all == 0 {
                self.dp -= 1
            } else {
                all += 1
                if self.nd < 800 {
                    self.d[self.nd] = c
                    self.nd += 1
                } else if c != 0x30 {
                    self.trunc = true
                }
            }
        } else {
            break
        }
        i += 1
    }
    if !saw_dot {
        self.dp = all
    }
    if i < n {
        i += 1 // e or E
        var esign := 1
        if b[i] == 0x2B || b[i] == 0x2D {
            if b[i] == 0x2D {
                esign = -1
            }
            i += 1
        }
        var e := 0
        for i < n {
            if e < ExpCap {
                e = e * 10 + Trunc[int](b[i] - 0x30)
            }
            i += 1
        }
        self.dp += e * esign
    }
}

// Trim drops trailing zero digits.
Decimal.Trim(mut self) {
    for self.nd > 0 && self.d[self.nd - 1] == 0x30 {
        self.nd -= 1
    }
    if self.nd == 0 {
        self.dp = 0
    }
}

// Assign sets the decimal to v.
Decimal.Assign(mut self, v: u64) {
    var buf: [24]u8 = [0; 24]
    var n := 0
    var x := v
    for x > 0 {
        q := x / 10
        buf[n] = Trunc[u8](x - q * 10) + 0x30
        n += 1
        x = q
    }
    self.nd = 0
    for k in 0..n {
        self.d[k] = buf[n - 1 - k]
    }
    self.nd = n
    self.dp = n
    self.Trim()
}

// Shift multiplies by 2^k, 60 bits per step (the step's digit products fit
// a u64).
Decimal.Shift(mut self, k: int) {
    if self.nd == 0 {
        return
    }
    var r := k
    if r > 0 {
        for r > 60 {
            self.LeftShift(60)
            r -= 60
        }
        self.LeftShift(r)
    } else if r < 0 {
        for r < -60 {
            self.RightShift(60)
            r += 60
        }
        self.RightShift(-r)
    }
}

// RightShift divides by 2^k, 1 <= k <= 60.
Decimal.RightShift(mut self, k: int) {
    var r := 0
    var w := 0
    var n: u64 = 0
    for n >> k == 0 {
        if r >= self.nd {
            if n == 0 {
                self.nd = 0
                return
            }
            for n >> k == 0 {
                n = n * 10
                r += 1
            }
            break
        }
        n = n * 10 + u64(self.d[r] - 0x30)
        r += 1
    }
    self.dp -= r - 1
    mask := (Trunc[u64](1) << k) - 1
    for r < self.nd {
        c := u64(self.d[r] - 0x30)
        dig := n >> k
        n = n & mask
        self.d[w] = Trunc[u8](dig) + 0x30
        w += 1
        n = n * 10 + c
        r += 1
    }
    for n > 0 {
        dig := n >> k
        n = n & mask
        if w < 800 {
            self.d[w] = Trunc[u8](dig) + 0x30
            w += 1
        } else if dig > 0 {
            self.trunc = true
        }
        n = n * 10
    }
    self.nd = w
    self.Trim()
}

// LeftShift multiplies by 2^k, 0 <= k <= 60.
Decimal.LeftShift(mut self, k: int) {
    // Both tables by reference: by value, each use copies the whole const at
    // compile time.
    deltas := &LeftDelta
    cutoffs := &LeftCutoff
    var delta := deltas[k]
    if DigitsBelow(self.d[0..self.nd], cutoffs[k]) {
        delta -= 1
    }
    var r := self.nd - 1
    var w := self.nd + delta
    var n: u64 = 0
    for r >= 0 {
        n += u64(self.d[r] - 0x30) << k
        q := n / 10
        rem := n - 10 * q
        w -= 1
        if w < 800 {
            self.d[w] = Trunc[u8](rem) + 0x30
        } else if rem != 0 {
            self.trunc = true
        }
        n = q
        r -= 1
    }
    for n > 0 {
        q := n / 10
        rem := n - 10 * q
        w -= 1
        if w < 800 {
            self.d[w] = Trunc[u8](rem) + 0x30
        } else if rem != 0 {
            self.trunc = true
        }
        n = q
    }
    self.nd += delta
    if self.nd >= 800 {
        self.nd = 800
    }
    self.dp += delta
    self.Trim()
}

// DigitsBelow reports whether the digits b are less than the digits s as
// a prefix comparison (Go's prefixIsLessThan).
DigitsBelow(b: []u8, s: str): bool {
    for i in 0..s.Len() {
        if i >= b.Len() {
            return true
        }
        if b[i] != s.Byte(i) {
            return b[i] < s.Byte(i)
        }
    }
    return false
}

// RoundUpAt reports whether rounding to nd digits goes up: above half, or
// exactly half and (digits were dropped, or the kept digit is odd).
Decimal.RoundUpAt(&self, nd: int): bool {
    if nd < 0 || nd >= self.nd {
        return false
    }
    if self.d[nd] == 0x35 && nd + 1 == self.nd {
        if self.trunc {
            return true
        }
        return nd > 0 && (self.d[nd - 1] - 0x30) % 2 != 0
    }
    return self.d[nd] >= 0x35
}

// Round rounds to nd digits, half to even.
Decimal.Round(mut self, nd: int) {
    if nd < 0 || nd >= self.nd {
        return
    }
    if self.RoundUpAt(nd) {
        var i := nd - 1
        for i >= 0 {
            if self.d[i] < 0x39 {
                self.d[i] += 1
                self.nd = i + 1
                return
            }
            i -= 1
        }
        self.d[0] = 0x31
        self.nd = 1
        self.dp += 1
        return
    }
    self.nd = nd
    self.Trim()
}

// RoundHalfUp rounds to nd digits, ties away from zero (the digits are a
// magnitude): up when the first dropped digit is 5 or more, for std.math's
// ExactDigits under ECMA-262's rounding. nd 0 keeps no digit: the
// value becomes 0, or 1 at the next power of ten.
Decimal.RoundHalfUp(mut self, nd: int) {
    if nd < 0 || nd >= self.nd {
        return
    }
    if self.d[nd] >= 0x35 {
        var i := nd - 1
        for i >= 0 {
            if self.d[i] < 0x39 {
                self.d[i] += 1
                self.nd = i + 1
                return
            }
            i -= 1
        }
        self.d[0] = 0x31
        self.nd = 1
        self.dp += 1
        return
    }
    self.nd = nd
    self.Trim()
}

// RoundedInteger is the value rounded to an integer, for dp <= 20.
Decimal.RoundedInteger(&self): u64 {
    if self.dp > 20 {
        return 0xFFFFFFFFFFFFFFFF
    }
    var n: u64 = 0
    var i := 0
    for i < self.dp && i < self.nd {
        n = n * 10 + u64(self.d[i] - 0x30)
        i += 1
    }
    for i < self.dp {
        n = n * 10
        i += 1
    }
    if self.RoundUpAt(self.dp) {
        n += 1
    }
    return n
}

// PowTab[n] is how far to shift a decimal whose point is n digits from
// where it must be (Go's powtab).
const PowTab: [9]int = [1, 3, 6, 9, 13, 16, 19, 23, 26]

// FloatBits64 is decimal.floatBits for f64: shift the value into [1/2, 1)
// times a power of two, then take 53 bits rounded half to even.
Decimal.FloatBits64(mut self): FloatBits {
    sign: u64 = if self.neg { 0x8000000000000000 } else { 0 }
    if self.nd == 0 || self.dp < -330 {
        return FloatBits(bits: sign, over: false)
    }
    if self.dp > 310 {
        return FloatBits(bits: sign | 0x7FF0000000000000, over: true)
    }
    pows := &PowTab
    var exp := 0
    for self.dp > 0 {
        var n := if self.dp < 9 { pows[self.dp] } else { 27 }
        self.Shift(-n)
        exp += n
    }
    for self.dp < 0 || self.dp == 0 && self.d[0] < 0x35 {
        n := if -self.dp < 9 { pows[-self.dp] } else { 27 }
        self.Shift(n)
        exp -= n
    }
    exp -= 1
    if exp < -1022 {
        n := -1022 - exp
        self.Shift(-n)
        exp += n
    }
    if exp + 1023 >= 0x7FF {
        return FloatBits(bits: sign | 0x7FF0000000000000, over: true)
    }
    self.Shift(53)
    var mant := self.RoundedInteger()
    if mant == 0x20000000000000 {
        mant = mant >> 1
        exp += 1
        if exp + 1023 >= 0x7FF {
            return FloatBits(bits: sign | 0x7FF0000000000000, over: true)
        }
    }
    if mant & 0x10000000000000 == 0 {
        exp = -1023
    }
    bits := (mant & 0xFFFFFFFFFFFFF) | (Trunc[u64](exp + 1023) << 52)
    return FloatBits(bits: sign | bits, over: false)
}

// BEGIN GENERATED LeftCheats by TestTextTablesUpToDate (std/std-text_tables_test.go); do not edit
// For a left shift by k: the digits it adds, one fewer when the
// number's leading digits are below the digits of 5^k (Go's
// decimal.go leftcheats).
const LeftDelta: [61]int = [
    0, 1, 1, 1, 2, 2, 2, 3, 3, 3, 4, 4, 4, 4, 5, 5, 5, 6, 6, 6, 7,
    7, 7, 7, 8, 8, 8, 9, 9, 9, 10, 10, 10, 10, 11, 11, 11, 12, 12, 12, 13,
    13, 13, 13, 14, 14, 14, 15, 15, 15, 16, 16, 16, 16, 17, 17, 17, 18, 18, 18, 19,
]

const LeftCutoff: [61]str = [
    "",
    "5",
    "25",
    "125",
    "625",
    "3125",
    "15625",
    "78125",
    "390625",
    "1953125",
    "9765625",
    "48828125",
    "244140625",
    "1220703125",
    "6103515625",
    "30517578125",
    "152587890625",
    "762939453125",
    "3814697265625",
    "19073486328125",
    "95367431640625",
    "476837158203125",
    "2384185791015625",
    "11920928955078125",
    "59604644775390625",
    "298023223876953125",
    "1490116119384765625",
    "7450580596923828125",
    "37252902984619140625",
    "186264514923095703125",
    "931322574615478515625",
    "4656612873077392578125",
    "23283064365386962890625",
    "116415321826934814453125",
    "582076609134674072265625",
    "2910383045673370361328125",
    "14551915228366851806640625",
    "72759576141834259033203125",
    "363797880709171295166015625",
    "1818989403545856475830078125",
    "9094947017729282379150390625",
    "45474735088646411895751953125",
    "227373675443232059478759765625",
    "1136868377216160297393798828125",
    "5684341886080801486968994140625",
    "28421709430404007434844970703125",
    "142108547152020037174224853515625",
    "710542735760100185871124267578125",
    "3552713678800500929355621337890625",
    "17763568394002504646778106689453125",
    "88817841970012523233890533447265625",
    "444089209850062616169452667236328125",
    "2220446049250313080847263336181640625",
    "11102230246251565404236316680908203125",
    "55511151231257827021181583404541015625",
    "277555756156289135105907917022705078125",
    "1387778780781445675529539585113525390625",
    "6938893903907228377647697925567626953125",
    "34694469519536141888238489627838134765625",
    "173472347597680709441192448139190673828125",
    "867361737988403547205962240695953369140625",
]
// END GENERATED LeftCheats
