// Why: a certificate's extensions decide what it may do (sign others, serve
// a host, which names); a verifier must read them strictly and refuse a
// critical one it does not understand.
//
// What: Ext and ReadExtensions, with one reader or checker per extension:
// KeyUsageOf, CheckSan, ReadConstraints, CheckSubtrees, CheckCrlPoints,
// CheckPolicies and CheckPolicyLimits.
//
// How: RFC 5280 4.2, validated as Go's crypto/x509 validates it. The
// extensions a check needs are read; the others are validated and ignored;
// a critical extension std.x509 does not handle sets FlagCritical, which
// Verify refuses. Details below.

// Read and validated: basic constraints (4.2.1.9), key usage (4.2.1.3),
// extended key usage (4.2.1.12), subjectAltName (4.2.1.6) and name
// constraints (4.2.1.10). Validated as Go's parser.go validates them and
// then ignored: key identifiers (4.2.1.1, 4.2.1.2), CRL distribution points
// (4.2.1.13), certificate policies (4.2.1.4), policy mappings (4.2.1.5),
// policy constraints (4.2.1.11), inhibitAnyPolicy (4.2.1.14) and authority
// information access (4.2.2.1), every OID read as Go reads it.
//
// std.x509 does no policy processing, so a critical policy mappings, policy
// constraints or inhibitAnyPolicy extension is unhandled, as in Go before
// 1.24. Any other extension marked critical, and a critical subjectAltName
// or name constraints extension holding only name forms std.x509 does not
// check, sets FlagCritical.

// Ext is what the extensions say.
struct Ext {
    flags: u32
    path_len: int
    key_usage: u32
    san: Span
    permitted: Span
    excluded: Span
}

// MaxExtensions bounds the extensions of one certificate, so the check
// for a repeated one stays O(1) memory.
const MaxExtensions: int = 64

// MaxPolicies bounds the policies of one certificate policies extension
// for the same reason (Go refuses a repeated policy too): 64 x 64 OID
// compares at most.
const MaxPolicies: int = 64

// Id-ce 2.5.29 (RFC 5280 4.2.1): an extension OID is these two bytes and
// one more.
const IdCe0: u8 = 0x55
const IdCe1: u8 = 0x1d
// id-pe-authorityInfoAccess 1.3.6.1.5.5.7.1.1.
const OidAia: [8]u8 = [0x2b, 0x06, 0x01, 0x05, 0x05, 0x07, 0x01, 0x01]
// id-kp-serverAuth 1.3.6.1.5.5.7.3.1 and anyExtendedKeyUsage 2.5.29.37.0.
const OidServerAuth: [8]u8 = [0x2b, 0x06, 0x01, 0x05, 0x05, 0x07, 0x03, 0x01]
const OidAnyEku: [4]u8 = [0x55, 0x1d, 0x25, 0x00]

// The GeneralName tags (RFC 5280 4.2.1.6) std.x509 checks.
const NameEmail: u32 = 1
const NameDns: u32 = 2
const NameUri: u32 = 6
const NameIp: u32 = 7

// ReadExtensions reads the Extensions SEQUENCE list.
ReadExtensions(der: []u8, list: &El): Malformed!Ext {
    return try ReadExtensionsBy(der, list, true)
}

// ReadExtensionsBy is ReadExtensions, reading each extension's elements
// in place when short_form is true and they are in the short form, else always
// with the cursor: the test's two walks.
ReadExtensionsBy(der: []u8, list: &El, short_form: bool): Malformed!Ext {
    var out := Ext(flags: 0, path_len: -1, key_usage: 0, san: NoSpan, permitted: NoSpan, excluded: NoSpan)
    var seen_at: [64]int = [0; 64]
    var seen_end: [64]int = [0; 64]
    var n := 0
    var c := Inside(der, list)
    for c.More() {
        // An extension's SEQUENCE { OID, BOOLEAN DEFAULT FALSE, OCTET STRING }
        // with every element in DER's short form and valid is read here, in
        // place; any other goes through the cursor, for its answer and its
        // offset.
        start := c.pos
        var oid_at := 0
        var oid_end := 0
        var critical := false
        var ve := El(tag: 4, class: 0, cons: false, start: 0, at: 0, end: 0)
        var short := false
        if short_form && start + 1 < c.end && der[start] == 0x30 && der[start + 1] < 0x80 && int(der[start + 1]) <= c.end - start - 2 {
            end := start + 2 + int(der[start + 1])
            q := start + 2
            if q + 1 < end && der[q] == 0x06 && der[q + 1] < 0x80 && int(der[q + 1]) <= end - q - 2 && OidValid(der[q + 2..q + 2 + int(der[q + 1])]) {
                at := q + 2
                var r := at + int(der[q + 1])
                oend := r
                var ok := true
                if r < end && (der[r] & 0xDF) == 0x01 {
                    // The BOOLEAN Peek(0, 1) finds: primitive, one byte, 0 or
                    // 0xFF.
                    ok = der[r] == 0x01 && r + 2 < end && der[r + 1] == 1 && (der[r + 2] == 0 || der[r + 2] == 0xFF)
                    if ok {
                        critical = der[r + 2] == 0xFF
                        r += 3
                    }
                }
                if ok && r + 1 < end && der[r] == 0x04 && der[r + 1] < 0x80 && int(der[r + 1]) == end - r - 2 {
                    oid_at = at
                    oid_end = oend
                    ve = El(tag: 4, class: 0, cons: false, start: r, at: r + 2, end: end)
                    c.pos = end
                    short = true
                }
            }
        }
        if !short {
            critical = false
            e := try c.Want(16, true)
            var ec := Inside(der, &e)
            oe := try ec.Want(6, false)
            if !OidValid(Content(der, &oe)) {
                fail Malformed(offset: oe.at)
            }
            if ec.Peek(0, 1) {
                be := try ec.Want(1, false)
                bb := Content(der, &be)
                if bb.Len() != 1 || (bb[0] != 0 && bb[0] != 0xFF) {
                    fail Malformed(offset: be.at)
                }
                critical = bb[0] == 0xFF
            }
            ve = try ec.Want(4, false)
            try ec.Finish()
            oid_at = oe.at
            oid_end = oe.end
        }
        o := der[oid_at..oid_end]
        if n == MaxExtensions {
            fail Malformed(offset: start)
        }
        for i in 0..n {
            if Same(o, der[seen_at[i]..seen_end[i]]) {
                fail Malformed(offset: start)
            }
        }
        seen_at[n] = oid_at
        seen_end[n] = oid_end
        n += 1
        handled := try ReadExtension(der, o, &ve, critical, mut out)
        if critical && !handled {
            out.flags |= FlagCritical
        }
    }
    return out
}

// ReadExtension reads one extension's value v into out, and is whether
// std.x509 handles it.
ReadExtension(der: []u8, o: []u8, v: &El, critical: bool, out: mut Ext): Malformed!bool {
    if o.Len() == OidAia.Len() && Same(o, OidAia[..]) {
        // RFC 5280 4.2.2.1: SEQUENCE OF AccessDescription { OID,
        // GeneralName }; a conforming CA marks it non-critical, and Go
        // refuses it critical.
        if critical {
            fail Malformed(offset: v.start)
        }
        var ac := Inside(der, v)
        list := try ac.Want(16, true)
        try ac.Finish()
        var lc := Inside(der, &list)
        for lc.More() {
            d := try lc.Want(16, true)
            var dc := Inside(der, &d)
            me := try dc.Want(6, false)
            if !OidValid(Content(der, &me)) {
                fail Malformed(offset: me.at)
            }
            _ = try dc.Next()
            try dc.Finish()
        }
        return true
    }
    if o.Len() != 3 || o[0] != IdCe0 || o[1] != IdCe1 {
        return false
    }
    var vc := Inside(der, v)
    id := o[2]
    if id == 19 {
        bc := try vc.Want(16, true)
        try vc.Finish()
        var bcc := Inside(der, &bc)
        out.flags |= FlagBasic
        if bcc.Peek(0, 1) {
            be := try bcc.Want(1, false)
            bb := Content(der, &be)
            if bb.Len() != 1 || (bb[0] != 0 && bb[0] != 0xFF) {
                fail Malformed(offset: be.at)
            }
            if bb[0] == 0xFF {
                out.flags |= FlagCa
            }
        }
        if bcc.More() {
            pe := try bcc.Want(2, false)
            p := SmallUint(Content(der, &pe))
            if p < 0 {
                fail Malformed(offset: pe.at)
            }
            out.path_len = Cast[int](p)
        }
        try bcc.Finish()
        return true
    }
    if id == 15 {
        ke := try vc.Want(3, false)
        try vc.Finish()
        out.flags |= FlagKeyUsage
        out.key_usage = try KeyUsageOf(der, &ke)
        return true
    }
    if id == 37 {
        se := try vc.Want(16, true)
        try vc.Finish()
        out.flags |= FlagEku
        var sc := Inside(der, &se)
        for sc.More() {
            ue := try sc.Want(6, false)
            u := Content(der, &ue)
            if !OidValid(u) {
                fail Malformed(offset: ue.at)
            }
            if Same(u, OidServerAuth[..]) {
                out.flags |= FlagEkuServer
            }
            if Same(u, OidAnyEku[..]) {
                out.flags |= FlagEkuAny
            }
        }
        return true
    }
    if id == 17 {
        se := try vc.Want(16, true)
        try vc.Finish()
        out.flags |= FlagSan
        out.san = Span(at: se.at, end: se.end)
        return try CheckSan(der, &se)
    }
    if id == 30 {
        out.flags |= FlagConstraints
        return try ReadConstraints(der, mut vc, out)
    }
    if id == 14 || id == 35 {
        // RFC 5280 4.2.1.1 and 4.2.1.2: a conforming CA marks the key
        // identifiers non-critical, and Go refuses them critical. The
        // subject's is an OCTET STRING; the authority's a SEQUENCE.
        if critical {
            fail Malformed(offset: v.start)
        }
        if id == 14 {
            _ = try vc.Want(4, false)
        } else {
            ae := try vc.Want(16, true)
            var ac := Inside(der, &ae)
            for ac.More() {
                _ = try ac.Next()
            }
        }
        try vc.Finish()
        return true
    }
    if id == 31 {
        try CheckCrlPoints(der, mut vc)
        return true
    }
    if id == 32 {
        try CheckPolicies(der, mut vc)
        return true
    }
    if id == 33 || id == 36 || id == 54 {
        // Validated, but not processed: unhandled when critical.
        try CheckPolicyLimits(der, id, mut vc)
        return false
    }
    return false
}

// CheckCrlPoints checks CRLDistributionPoints (RFC 5280 4.2.1.13) as Go
// reads it: a SEQUENCE OF DistributionPoint, each a SEQUENCE whose
// distributionPoint [0], when present, holds a fullName [0] (Go refuses
// nameRelativeToCRLIssuer) of well-formed elements.
CheckCrlPoints(der: []u8, vc: mut Cur): Malformed!void {
    list := try vc.Want(16, true)
    try vc.Finish()
    var lc := Inside(der, &list)
    for lc.More() {
        dp := try lc.Want(16, true)
        var dc := Inside(der, &dp)
        if !dc.Peek(2, 0) {
            continue
        }
        n0 := try dc.Next()
        if !n0.cons {
            // Not Go's [0] constructed: an absent name to Go.
            continue
        }
        var nc := Inside(der, &n0)
        full := try nc.Next()
        if full.class != 2 || full.tag != 0 || !full.cons {
            fail Malformed(offset: full.start)
        }
        var fc := Inside(der, &full)
        for fc.More() {
            _ = try fc.Next()
        }
    }
}

// CheckPolicies checks certificatePolicies (RFC 5280 4.2.1.4) as Go's
// parseCertificatePoliciesExtension does: a SEQUENCE OF PolicyInformation,
// each a SEQUENCE whose first element is an OID of any arc size
// (newOIDFromDER), no OID twice; the qualifiers are not read. More than
// MaxPolicies is Malformed, so the repeat check is O(1) memory.
CheckPolicies(der: []u8, vc: mut Cur): Malformed!void {
    var seen_at: [64]int = [0; 64]
    var seen_end: [64]int = [0; 64]
    var n := 0
    list := try vc.Want(16, true)
    try vc.Finish()
    var lc := Inside(der, &list)
    for lc.More() {
        pi := try lc.Want(16, true)
        var pc := Inside(der, &pi)
        oe := try pc.Want(6, false)
        o := Content(der, &oe)
        if !OidDerValid(o) {
            fail Malformed(offset: oe.at)
        }
        if n == MaxPolicies {
            fail Malformed(offset: pi.start)
        }
        for i in 0..n {
            if Same(o, der[seen_at[i]..seen_end[i]]) {
                fail Malformed(offset: pi.start)
            }
        }
        seen_at[n] = oe.at
        seen_end[n] = oe.end
        n += 1
    }
}

// CheckPolicyLimits checks policy mappings (33: a SEQUENCE OF SEQUENCE {
// OID, OID }), policy constraints (36: a SEQUENCE of an optional [0] and
// [1] INTEGER) or inhibitAnyPolicy (54: an INTEGER), each INTEGER minimal
// and within 64 bits, as Go's processExtensions reads them.
CheckPolicyLimits(der: []u8, id: u8, vc: mut Cur): Malformed!void {
    if id == 54 {
        ie := try vc.Want(2, false)
        try vc.Finish()
        if !SmallInt(Content(der, &ie)) {
            fail Malformed(offset: ie.at)
        }
        return
    }
    se := try vc.Want(16, true)
    try vc.Finish()
    var sc := Inside(der, &se)
    if id == 33 {
        for sc.More() {
            m := try sc.Want(16, true)
            var mc := Inside(der, &m)
            _ = try mc.Want(6, false)
            _ = try mc.Want(6, false)
        }
        return
    }
    // Go peeks for exactly 0x80, then 0x81 (primitive [0] and [1]).
    for k in 0..2 {
        if sc.More() && der[sc.pos] == (0x80 | Trunc[u8](k)) {
            ke := try sc.Next()
            if !SmallInt(Content(der, &ke)) {
                fail Malformed(offset: ke.at)
            }
        }
    }
}

// SmallInt is whether b is a minimal INTEGER of at most 8 bytes, as
// cryptobyte reads an int64.
SmallInt(b: []u8): bool {
    return MinimalInt(b) && b.Len() <= 8
}

// KeyUsageOf is a KeyUsage BIT STRING's bits, RFC 5280's bit n as 1 << n.
KeyUsageOf(der: []u8, e: &El): Malformed!u32 {
    b := Content(der, e)
    if !BitsValid(b) {
        fail Malformed(offset: e.at)
    }
    var u: u32 = 0
    for i in 0..9 {
        k := 1 + i / 8
        if k < b.Len() && ((b[k] >> Trunc[u8](7 - i % 8)) & 1) != 0 {
            u |= u32(1) << Trunc[u32](i)
        }
    }
    return u
}

// Ia5 is whether b is IA5 text (ASCII).
Ia5(b: []u8): bool {
    for x in b {
        if x >= 0x80 {
            return false
        }
    }
    return true
}

// CheckSan checks GeneralNames s as Go reads a subjectAltName: an email,
// DNS name or URI is IA5, an IP address 4 or 16 bytes and not IPv4-mapped.
// It is whether the extension names a form std.x509 checks.
CheckSan(der: []u8, s: &El): Malformed!bool {
    var c := Inside(der, s)
    var any := false
    for c.More() {
        g := try c.Next()
        if g.class != 2 || g.cons {
            continue
        }
        b := Content(der, &g)
        if g.tag == NameEmail || g.tag == NameDns || g.tag == NameUri {
            if !Ia5(b) {
                fail Malformed(offset: g.at)
            }
            any = true
        } else if g.tag == NameIp {
            if b.Len() != 4 && (b.Len() != 16 || Mapped4(b)) {
                fail Malformed(offset: g.at)
            }
            any = true
        }
    }
    return any
}

// Mapped4 is whether a 16-byte address is IPv4-mapped (::ffff:a.b.c.d).
Mapped4(b: []u8): bool {
    for i in 0..10 {
        if b[i] != 0 {
            return false
        }
    }
    return b[10] == 0xFF && b[11] == 0xFF
}

// ReadConstraints reads NameConstraints (RFC 5280 4.2.1.10) from vc,
// validating each subtree's base as Go does, and is whether every base is
// a form std.x509 checks (DNS, IP, email, URI).
ReadConstraints(der: []u8, vc: mut Cur, out: mut Ext): Malformed!bool {
    nc := try vc.Want(16, true)
    try vc.Finish()
    var c := Inside(der, &nc)
    var any := false
    var handled := true
    if c.Peek(2, 0) {
        pe := try c.Next()
        if !pe.cons {
            fail Malformed(offset: pe.start)
        }
        if pe.end == pe.at {
            fail Malformed(offset: pe.start)
        }
        out.permitted = Span(at: pe.at, end: pe.end)
        handled = try CheckSubtrees(der, &pe) && handled
        any = pe.end > pe.at
    }
    if c.Peek(2, 1) {
        xe := try c.Next()
        if !xe.cons {
            fail Malformed(offset: xe.start)
        }
        if xe.end == xe.at {
            fail Malformed(offset: xe.start)
        }
        out.excluded = Span(at: xe.at, end: xe.end)
        handled = try CheckSubtrees(der, &xe) && handled
        any = any || xe.end > xe.at
    }
    try c.Finish()
    if !any {
        fail Malformed(offset: nc.start)
    }
    return handled
}

// CheckSubtrees checks GeneralSubtrees t: each a SEQUENCE whose first
// element is the base. It is whether every base is a form std.x509 checks.
CheckSubtrees(der: []u8, t: &El): Malformed!bool {
    var c := Inside(der, t)
    var handled := true
    for c.More() {
        st := try c.Want(16, true)
        var sc := Inside(der, &st)
        g := try sc.Next()
        b := Content(der, &g)
        if g.class != 2 || g.cons {
            handled = false
            continue
        }
        if g.tag == NameDns {
            if !Ia5(b) || !DomainValid(b, true) {
                fail Malformed(offset: g.at)
            }
        } else if g.tag == NameIp {
            if b.Len() != 8 && b.Len() != 32 {
                fail Malformed(offset: g.at)
            }
            half := b.Len() / 2
            if !MaskValid(b[half..]) || (half == 16 && Mapped4(b[..16])) {
                fail Malformed(offset: g.at)
            }
        } else if g.tag == NameEmail {
            if !Ia5(b) {
                fail Malformed(offset: g.at)
            }
            valid := if HasByte(b, b'@') { MailboxValid(b) } else { DomainValid(b, true) }
            if !valid {
                fail Malformed(offset: g.at)
            }
        } else if g.tag == NameUri {
            if !Ia5(b) || IpOf(b).IsSome() || !DomainValid(b, true) {
                fail Malformed(offset: g.at)
            }
        } else {
            handled = false
        }
    }
    return handled
}

// HasByte is whether b holds x.
HasByte(b: []u8, x: u8): bool {
    for y in b {
        if y == x {
            return true
        }
    }
    return false
}

// MaskValid is whether m is ones followed by zeros.
MaskValid(m: []u8): bool {
    var zero := false
    for b in m {
        if zero {
            if b != 0 {
                return false
            }
            continue
        }
        if b == 0xFF {
            continue
        }
        if b != 0x00 && b != 0x80 && b != 0xC0 && b != 0xE0 && b != 0xF0 && b != 0xF8 && b != 0xFC && b != 0xFE {
            return false
        }
        zero = true
    }
    return true
}

// DomainValid is Go's domainNameValid: empty is valid; no trailing dot;
// in a constraint one leading dot is allowed; every byte printable ASCII
// other than space; no empty label.
DomainValid(s: []u8, constraint: bool): bool {
    if s.Len() == 0 {
        return true
    }
    if s[s.Len() - 1] == b'.' {
        return false
    }
    var from := 0
    if constraint && s[0] == b'.' {
        from = 1
    }
    var label := 0
    for i in from..s.Len() {
        x := s[i]
        if x < 33 || x > 126 {
            return false
        }
        if x == b'.' {
            if label == 0 {
                return false
            }
            label = 0
        } else {
            label += 1
        }
    }
    return label > 0
}
