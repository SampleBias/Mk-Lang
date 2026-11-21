// Why: a certificate parser reads bytes from the network; it must report
// every fault at its byte and use bounded memory whatever the input.
//
// What: Parse, RFC 5280 4.1's Certificate and TBSCertificate; Cur, the
// cursor over std.encoding's DER reader; and the field checks CheckName,
// CheckSpki, TimeOf, MinimalInt and OidValid.
//
// How: one pass with the cursor, the structure checked as Go's crypto/x509
// checks it, and the extensions a check needs read and validated here, so
// Verify meets no malformed extension. Details below.

// Go's checks: the inner and outer signature algorithms agree, the version
// is 1 to 3, the serial is not negative, extensions appear only in version
// 3 and once each. A public key is kept as its SubjectPublicKeyInfo and
// parsed when a signature is checked, so a certificate with a key
// std.crypto does not take (RSA below 2048 bits) still parses, as a system
// bundle may hold one.

import std.encoding
import std.time

// Cur reads the elements of der[pos..end] in order; every offset it
// reports is into der.
struct Cur {
    der: []u8
    pos: int
    end: int
}

// El is one element: its tag number, class and constructed bit, the
// offset of its identifier, and its content der[at..end].
struct El {
    tag: u32
    class: u8
    cons: bool
    start: int
    at: int
    end: int
}

// Inside is a cursor over e's content.
Inside(der: []u8, e: &El): Cur from der {
    return Cur(der: der, pos: e.at, end: e.end)
}

// More is whether an element is left.
Cur.More(&self): bool {
    return self.pos < self.end
}

// Where is the offset an encoding error names, 0 when it names none.
Where(e: encoding.Error): int {
    return match e {
        encoding.BadInput(offset) { offset }
        _ { 0 }
    }
}

// Next reads the next element, failing at the byte where std.encoding's
// reader stops (a length past the end of the enclosing element included).
// The short form, a one-byte identifier and a one-byte length that fits,
// is DER as it stands (X.690 8.1.2.2, 8.1.3.4) and is most of a
// certificate's elements: Next reads it here, as std.encoding's reader
// reads it, and hands every other form, and every fault, to NextDer.
Cur.Next(mut self): Malformed!El {
    if self.pos + 1 < self.end {
        id := self.der[self.pos]
        len := self.der[self.pos + 1]
        if (id & 0x1F) != 0x1F && len < 0x80 && int(len) <= self.end - self.pos - 2 {
            at := self.pos + 2
            el := El(tag: u32(id & 0x1F), class: id >> 6, cons: id & 0x20 != 0, start: self.pos, at: at, end: at + int(len))
            self.pos = at + int(len)
            return el
        }
    }
    return try self.NextDer()
}

// NextDer is Next through std.encoding's reader, for every form.
Cur.NextDer(mut self): Malformed!El {
    if self.pos >= self.end {
        fail Malformed(offset: self.end)
    }
    // X.690 8.1.2.4's high-tag-number form: no X.509 structure has a tag
    // above 30, and Go's cryptobyte reads none.
    if (self.der[self.pos] & 0x1F) == 0x1F {
        fail Malformed(offset: self.pos)
    }
    got := encoding.DerRead(self.der[self.pos..self.end]) else e {
        fail Malformed(offset: self.pos + Where(e))
    }
    n := got.value.content.Len()
    at := self.end - got.rest.Len() - n
    el := El(tag: got.value.tag, class: got.value.class, cons: got.value.constructed, start: self.pos, at: at, end: at + n)
    self.pos = at + n
    return el
}

// Want reads the next element and fails unless it is universal with tag
// number tag and the constructed bit cons.
Cur.Want(mut self, tag: u32, cons: bool): Malformed!El {
    // Next's short form, repeated here so the element is built once, in
    // the result, and not copied out of Next's.
    if self.pos + 1 < self.end {
        id := self.der[self.pos]
        len := self.der[self.pos + 1]
        if (id & 0x1F) != 0x1F && len < 0x80 && int(len) <= self.end - self.pos - 2 {
            start := self.pos
            at := start + 2
            self.pos = at + int(len)
            if (id >> 6) != 0 || u32(id & 0x1F) != tag || (id & 0x20 != 0) != cons {
                fail Malformed(offset: start)
            }
            return El(tag: tag, class: 0, cons: cons, start: start, at: at, end: at + int(len))
        }
    }
    e := try self.Next()
    if e.class != 0 || e.tag != tag || e.cons != cons {
        fail Malformed(offset: e.start)
    }
    return e
}

// Peek is whether the next element has class class and low tag number tag
// (below 31: every tag std.x509 peeks for).
Cur.Peek(&self, class: u8, tag: u32): bool {
    if self.pos >= self.end {
        return false
    }
    b := self.der[self.pos]
    return (b >> 6) == class && u32(b & 0x1F) == tag
}

// End fails unless every element was read.
Cur.Finish(&self): Malformed!void {
    if self.pos < self.end {
        fail Malformed(offset: self.pos)
    }
}

// Content is e's content octets.
Content(der: []u8, e: &El): []u8 from der {
    return der[e.at..e.end]
}

// SmallUint is the value of a non-negative minimal INTEGER of at most 8
// bytes below 2^62, or -1.
SmallUint(b: []u8): i64 {
    if b.Len() == 0 || b.Len() > 8 || (b[0] & 0x80) != 0 {
        return -1
    }
    if b.Len() > 1 && b[0] == 0 && (b[1] & 0x80) == 0 {
        return -1
    }
    var v: i64 = 0
    for x in b {
        if v >= (1 << 54) {
            return -1
        }
        v = (v << 8) | i64(x)
    }
    return v
}

// MinimalInt is whether b is a minimal two's complement INTEGER (X.690
// 8.3.2).
MinimalInt(b: []u8): bool {
    if b.Len() == 0 {
        return false
    }
    if b.Len() > 1 && (b[0] == 0 && (b[1] & 0x80) == 0 || b[0] == 0xFF && (b[1] & 0x80) != 0) {
        return false
    }
    return true
}

/// Reads one DER certificate with nothing after it. The result is a set of
/// views of der, so der must outlive it. O(der) with no recursion;
/// allocates nothing; never panics on input.
///
/// Fails with Malformed at the first byte that is not RFC 5280's
/// structure: a DER fault, a version above 3, a negative serial, inner and
/// outer signature algorithms that differ, a validity time that is not
/// UTCTime or GeneralizedTime in seconds with Z, or an extension outside
/// version 3 or repeated. These extensions fail it when malformed: basic
/// constraints, key usage, extended key usage, subjectAltName, name
/// constraints, key identifier, CRL distribution points, certificate
/// policies (more than 64 included), policy mappings, policy constraints,
/// inhibitAnyPolicy and authority information access. So do an object
/// identifier that is not minimal, a constructed or malformed unique
/// identifier, and a key identifier or authority information access
/// extension marked critical. A time outside time.Time's range saturates.
export Parse(a: &Arena, der: []u8): Error!Certificate from a, der {
    _ = a
    return try ParseDer(der)
}

// ParseDer is Parse's body.
ParseDer(der: []u8): Malformed!Certificate {
    var top := Cur(der: der, pos: 0, end: der.Len())
    whole := try top.Want(16, true)
    try top.Finish()
    var c := Inside(der, &whole)
    tbs := try c.Want(16, true)
    alg := try c.Want(16, true)
    sig := try c.Want(3, false)
    try c.Finish()
    if sig.end == sig.at || der[sig.at] != 0 {
        fail Malformed(offset: sig.at)
    }
    var t := Inside(der, &tbs)
    var version := 0
    if t.Peek(2, 0) {
        ve := try t.Next()
        if !ve.cons {
            fail Malformed(offset: ve.start)
        }
        var vc := Inside(der, &ve)
        vi := try vc.Want(2, false)
        try vc.Finish()
        vb := Content(der, &vi)
        if vb.Len() != 1 || vb[0] > 2 {
            fail Malformed(offset: vi.at)
        }
        version = int(vb[0])
    }
    serial := try t.Want(2, false)
    sb := Content(der, &serial)
    if !MinimalInt(sb) || (sb[0] & 0x80) != 0 {
        fail Malformed(offset: serial.at)
    }
    inner := try t.Want(16, true)
    if !Same(der[inner.start..inner.end], der[alg.start..alg.end]) {
        fail Malformed(offset: inner.start)
    }
    code := try SigAlgOf(der, &alg)
    issuer := try t.Want(16, true)
    try CheckName(der, &issuer)
    validity := try t.Want(16, true)
    var vc := Inside(der, &validity)
    nb := try vc.Next()
    not_before := try TimeOf(der, &nb)
    na := try vc.Next()
    not_after := try TimeOf(der, &na)
    try vc.Finish()
    subject := try t.Want(16, true)
    try CheckName(der, &subject)
    spki := try t.Want(16, true)
    try CheckSpki(der, &spki)
    if version >= 1 {
        // issuerUniqueID [1] and subjectUniqueID [2] (RFC 5280 4.1.2.8):
        // IMPLICIT BIT STRING, so primitive. A constructed one is not
        // DER, and reading past it would take extensions Go never reads.
        for k in 1..3 {
            if t.Peek(2, Cast[u32](k)) {
                u := try t.Next()
                if u.cons || !BitsValid(Content(der, &u)) {
                    fail Malformed(offset: u.start)
                }
            }
        }
    }
    var ext := Ext(flags: 0, path_len: -1, key_usage: 0, san: NoSpan, permitted: NoSpan, excluded: NoSpan)
    if version == 2 && t.Peek(2, 3) {
        ee := try t.Next()
        if !ee.cons {
            fail Malformed(offset: ee.start)
        }
        var ec := Inside(der, &ee)
        list := try ec.Want(16, true)
        try ec.Finish()
        ext = try ReadExtensions(der, &list)
    }
    try t.Finish()
    return Certificate(data: der, tbs_span: Span(at: tbs.start, end: tbs.end), sig_span: Span(at: sig.at + 1, end: sig.end), issuer_span: Span(at: issuer.start, end: issuer.end), subject_span: Span(at: subject.start, end: subject.end), spki_span: Span(at: spki.start, end: spki.end), san_span: ext.san, permitted_span: ext.permitted, excluded_span: ext.excluded, sig_alg: code, valid_from: not_before, valid_to: not_after, flags: ext.flags, path_len: ext.path_len, key_usage: ext.key_usage)
}

// CheckName checks a Name (RFC 5280 4.1.2.4): a SEQUENCE OF non-empty SETs
// of SEQUENCE { OID, value }, each OID minimal with arcs below 2^31 and
// each value a string Go's parseASN1String takes.
CheckName(der: []u8, n: &El): Malformed!void {
    if NameShort(der, n) {
        return
    }
    try CheckNameCur(der, n)
}

// CheckNameCur is CheckName by the cursor, for every form.
CheckNameCur(der: []u8, n: &El): Malformed!void {
    var rdns := Inside(der, n)
    for rdns.More() {
        set := try rdns.Want(17, true)
        var atvs := Inside(der, &set)
        if !atvs.More() {
            fail Malformed(offset: set.start)
        }
        for atvs.More() {
            atv := try atvs.Want(16, true)
            var parts := Inside(der, &atv)
            oe := try parts.Want(6, false)
            if !OidValid(Content(der, &oe)) {
                fail Malformed(offset: oe.at)
            }
            ve := try parts.Next()
            if ve.class != 0 || ve.cons || !StringTag(ve.tag) || !StringValid(ve.tag, Content(der, &ve)) {
                fail Malformed(offset: ve.start)
            }
            try parts.Finish()
        }
    }
}

// NameShort is whether the cursor walk of CheckName passes over n with
// every element in DER's short form (Cur.Next's own case): the same reads
// and the same checks, in one loop over the bytes. false for any other
// Name, which CheckName then walks with the cursor, for its answer and its
// offset.
NameShort(der: []u8, n: &El): bool {
    var p := n.at
    end := n.end
    for p < end {
        // A non-empty SET.
        if p + 1 >= end || der[p] != 0x31 || der[p + 1] >= 0x80 || int(der[p + 1]) > end - p - 2 || der[p + 1] == 0 {
            return false
        }
        set_end := p + 2 + int(der[p + 1])
        var q := p + 2
        p = set_end
        for q < set_end {
            // A SEQUENCE of an OID and a string, and nothing more.
            if q + 1 >= set_end || der[q] != 0x30 || der[q + 1] >= 0x80 || int(der[q + 1]) > set_end - q - 2 {
                return false
            }
            atv_end := q + 2 + int(der[q + 1])
            o := q + 2
            q = atv_end
            if o + 1 >= atv_end || der[o] != 0x06 || der[o + 1] >= 0x80 || int(der[o + 1]) > atv_end - o - 2 {
                return false
            }
            v := o + 2 + int(der[o + 1])
            if !OidValid(der[o + 2..v]) {
                return false
            }
            if v + 1 >= atv_end || der[v + 1] >= 0x80 || int(der[v + 1]) != atv_end - v - 2 {
                return false
            }
            id := der[v]
            tag := u32(id & 0x1F)
            if (id & 0xE0) != 0 || tag == 0x1F || !StringTag(tag) || !StringValid(tag, der[v + 2..atv_end]) {
                return false
            }
        }
    }
    return true
}

// CheckSpki checks a SubjectPublicKeyInfo (RFC 5280 4.1.2.7): SEQUENCE {
// AlgorithmIdentifier, BIT STRING }.
CheckSpki(der: []u8, s: &El): Malformed!void {
    var c := Inside(der, s)
    ai := try c.Want(16, true)
    var ac := Inside(der, &ai)
    oe := try ac.Want(6, false)
    if !OidValid(Content(der, &oe)) {
        fail Malformed(offset: oe.at)
    }
    if ac.More() {
        _ = try ac.Next()
    }
    try ac.Finish()
    key := try c.Want(3, false)
    try c.Finish()
    if key.end == key.at || der[key.at] != 0 {
        fail Malformed(offset: key.at)
    }
}

// TimeOf reads a validity time (RFC 5280 4.1.2.5): UTCTime YYMMDDHHMMSSZ,
// its year 1950 to 2049, or GeneralizedTime YYYYMMDDHHMMSSZ, each field
// in its range. The instant saturates at the ends of time.Time's range,
// so 99991231235959Z, RFC 5280's no-expiry value, is the latest Time.
TimeOf(der: []u8, e: &El): Malformed!time.Time {
    b := Content(der, e)
    if e.class != 0 || e.cons || (e.tag != 23 && e.tag != 24) {
        fail Malformed(offset: e.start)
    }
    long := e.tag == 24
    want := if long { 15 } else { 13 }
    if b.Len() != want || b[want - 1] != b'Z' {
        fail Malformed(offset: e.at)
    }
    for i in 0..want - 1 {
        if b[i] < b'0' || b[i] > b'9' {
            fail Malformed(offset: e.at + i)
        }
    }
    var y := 0
    var i := 0
    if long {
        y = Two(b, 0) * 100 + Two(b, 2)
        i = 4
    } else {
        y = Two(b, 0)
        y += if y < 50 { 2000 } else { 1900 }
        i = 2
    }
    month := Two(b, i)
    day := Two(b, i + 2)
    hour := Two(b, i + 4)
    minute := Two(b, i + 6)
    second := Two(b, i + 8)
    if month < 1 || month > 12 || day < 1 || day > MonthDays(y, month) || hour > 23 || minute > 59 || second > 59 {
        fail Malformed(offset: e.at)
    }
    d := time.Date(year: Cast[i32](y), month: month, day: day, hour: hour, minute: minute, second: second, nanos: 0, weekday: 0, year_day: 0, offset_seconds: 0)
    return d.Time()
}

// Two is the decimal number b[at..at + 2], both bytes digits.
Two(b: []u8, at: int): int {
    return int(b[at] - b'0') * 10 + int(b[at + 1] - b'0')
}

// MonthDays is the length of month m of the proleptic Gregorian year y.
MonthDays(y: int, m: int): int {
    if m == 2 {
        leap := y % 4 == 0 && (y % 100 != 0 || y % 400 == 0)
        return if leap { 29 } else { 28 }
    }
    if m == 4 || m == 6 || m == 9 || m == 11 {
        return 30
    }
    return 31
}

// StringTag is whether tag is a string type a Name value may have:
// UTF8String 12, NumericString 18, PrintableString 19, T61String 20,
// IA5String 22, BMPString 30.
StringTag(tag: u32): bool {
    return tag == 12 || tag == 18 || tag == 19 || tag == 20 || tag == 22 || tag == 30
}

// PrintableBits is PrintableString's set (X.680 41.4: letters, digits,
// space and ' ( ) + , - . / : = ?) with '*' and '&', which Go also takes,
// as a bitmap of the 128 ASCII bytes: byte x is bit x & 7 of entry x >> 3.
const PrintableBits: [16]u8 = [0x00, 0x00, 0x00, 0x00, 0xC1, 0xFF, 0xFF, 0xA7, 0xFE, 0xFF, 0xFF, 0x07, 0xFE, 0xFF, 0xFF, 0x07]

// StringValid is Go's parseASN1String's check of a value of type tag:
// UTF-8 for UTF8String, digits and space for NumericString, the printable
// set (with '*' and '&') for PrintableString, ASCII for IA5String, an even
// length with no surrogate or noncharacter for BMPString, anything for
// T61String.
StringValid(tag: u32, b: []u8): bool {
    if tag == 12 {
        _ = str.From(b) else {
            return false
        }
        return true
    }
    if tag == 30 {
        if b.Len() % 2 != 0 {
            return false
        }
        var n := b.Len()
        if n >= 2 && b[n - 1] == 0 && b[n - 2] == 0 {
            n -= 2
        }
        var i := 0
        for i < n {
            p := (u32(b[i]) << 8) | u32(b[i + 1])
            if p == 0xFFFE || p == 0xFFFF || (p >= 0xFDD0 && p <= 0xFDEF) || (p >= 0xD800 && p <= 0xDFFF) {
                return false
            }
            i += 2
        }
        return true
    }
    for x in b {
        ok := if tag == 18 {
            (x >= b'0' && x <= b'9') || x == b' '
        } else if tag == 19 {
            x < 0x80 && ((PrintableBits[int(x >> 3)] >> (x & 7)) & 1) != 0
        } else if tag == 22 {
            x < 0x80
        } else {
            true
        }
        if !ok {
            return false
        }
    }
    return true
}

// BitsValid is whether b is a DER BIT STRING's content (X.690 8.6.2,
// 11.2): the unused-bit count first, at most 7, 0 when no byte follows,
// and those bits of the last byte zero.
BitsValid(b: []u8): bool {
    if b.Len() == 0 || b[0] > 7 || (b.Len() == 1 && b[0] != 0) {
        return false
    }
    return b.Len() == 1 || (b[b.Len() - 1] & ((u8(1) << b[0]) - 1)) == 0
}

// OidDerValid is whether o is an OID's content as Go's newOIDFromDER
// reads a certificate policy: non-empty, each arc minimal (no leading
// 0x80), the last byte ending an arc; an arc may be any size.
OidDerValid(o: []u8): bool {
    if o.Len() == 0 || (o[o.Len() - 1] & 0x80) != 0 {
        return false
    }
    var start := true
    for x in o {
        if start && x == 0x80 {
            return false
        }
        start = (x & 0x80) == 0
    }
    return true
}

// OidValid is whether o is an OID's content as Go's cryptobyte reads one:
// non-empty, each arc minimal (no leading 0x80) and below 2^31, the last
// byte ending an arc.
OidValid(o: []u8): bool {
    if o.Len() == 0 || (o[o.Len() - 1] & 0x80) != 0 {
        return false
    }
    var v: u64 = 0
    var start := true
    for x in o {
        if start && x == 0x80 {
            return false
        }
        v = (v << 7) | u64(x & 0x7F)
        if v >= (1 << 31) {
            return false
        }
        start = (x & 0x80) == 0
        if start {
            v = 0
        }
    }
    return true
}
