// Why: most URLs a program parses are already in their serialized form,
// and the full state machine spends most of its time proving that.
//
// What: ParsePlain, the basic URL parser's answer in one pass for an input
// that is its own serialization (but for capitals in its scheme and host),
// with its byte table (HostKinds), LowerCopy, and PathRunEnd, the path
// scan it shares with parse.mk's Parser.PathRun.
//
// How: one scan checks the whole form; any input outside it gets none and
// goes to the state machine in parse.mk, which gives every answer.

import std.rt

// The kinds of byte ParsePlain's host loop tells apart: a byte a plain
// domain holds (IsPlainDomain's), an ASCII capital, which the domain
// parser lower-cases, and any other byte, which ends the host.
const HostEnd: u8 = 0
const HostPlain: u8 = 1
const HostCapital: u8 = 2

// HostTable is HostKinds: each byte's kind.
HostTable(): [256]u8 {
    var t: [256]u8 = [0; 256]
    for i in 0..0x80 {
        c := Trunc[u8](i)
        if c >= b'A' && c <= b'Z' {
            t[i] = HostCapital
        } else if !IsClass(c, ForbiddenDomain) {
            t[i] = HostPlain
        }
    }
    return t
}

const HostKinds: [256]u8 = HostTable()

// ParsePlain is the basic URL parser's answer for an input whose href is
// the input itself with the capitals of its scheme and host lower-cased,
// or none for any other input, which the caller then parses. That input
// is a special scheme other than "file", "://", a host that is a plain
// domain (IsPlainDomain but for capitals) not ending in a number or an
// IPv4 address as the serializer writes it, a port as the serializer
// writes it and not the scheme's default, if any, and '/' and a path
// PathRunEnd takes whole, then a query and a fragment, if any, with no
// byte their sets encode. Every byte of it is ASCII above the space, so
// ParseWith's steps 1 to 3 change nothing; the states it runs through
// (scheme, special authority slashes and ignore slashes, authority, host,
// port, path start, path with PathRun, query, fragment) each take their
// part as it is, none reads a base, and the serializer writes the parts
// back in order. O(n); allocates the href, or nothing.
ParsePlain(a: &Arena, input: str into a): OutOfMemory!?Url from a {
    s := input.AsBytes()
    n := s.Len()
    // The scheme: two to five letters and ':'.
    var i := 0
    var upper := false
    for i < n && i < 6 && IsAlpha(s[i]) {
        upper = upper || s[i] < b'a'
        i += 1
    }
    if i < 2 || i > 5 || i >= n || s[i] != b':' {
        return none
    }
    scheme_end := i
    var low: [5]u8 = [0; 5]
    for k in 0..scheme_end {
        low[k] = s[k] | 0x20
    }
    scheme := low[0..scheme_end]
    if !IsSpecialScheme(scheme) || SameText(scheme, "file") {
        return none
    }
    dport := DefaultPort(scheme)
    // "//" and a host that is not empty.
    if scheme_end + 3 >= n || s[scheme_end + 1] != b'/' || s[scheme_end + 2] != b'/' {
        return none
    }
    host_start := scheme_end + 3
    var o := host_start
    var kinds: u8 = 0
    for o < n {
        k := HostKinds[int(s[o])]
        if k == HostEnd {
            break
        }
        kinds |= k
        o += 1
    }
    host_end := o
    if host_end == host_start {
        return none
    }
    host := s[host_start..host_end]
    // The domain parser's question, asked of the bytes it asks it of.
    if !IdnaAsciiAllowed(host) {
        return none
    }
    var ip := false
    var addr: u32 = 0
    if EndsInNumber(host) {
        // Only an address written as the IPv4 serializer writes it.
        v := ParseIpv4(host) else {
            return none
        }
        var text: [48]u8 = [0; 48]
        hv := HostValue.Ipv4(addr: v)
        k := IpText(&hv, mut text[..])
        if !Same(text[0..k], host) {
            return none
        }
        ip = true
        addr = v
    }
    upper = upper || kinds & HostCapital != 0
    // The port: one to five digits, no leading zero but in "0", below
    // 2^16 and not the scheme's default.
    var port: ?u16 = none
    if o < n && s[o] == b':' {
        o += 1
        digits := o
        var v := 0
        for o < n && o - digits < 6 && IsDigit(s[o]) {
            v = v * 10 + int(s[o] - b'0')
            o += 1
        }
        k := o - digits
        if k == 0 || k > 5 || k > 1 && s[digits] == b'0' || v > 0xFFFF {
            return none
        }
        if SamePort(some(Trunc[u16](v)), dport) {
            return none
        }
        port = some(Trunc[u16](v))
    }
    // The path: '/' and a run of segments PathRunEnd takes.
    if o >= n || s[o] != b'/' {
        return none
    }
    path_start := o
    path_end := PathRunEnd(s, o + 1)
    if path_end < 0 {
        return none
    }
    o = path_end
    var query_start := -1
    var query_end := -1
    if o < n && s[o] == b'?' {
        o += 1
        query_start = o
        for o < n && s[o] != b'#' {
            if Encodes(s[o], InSpecialQuery) {
                return none
            }
            o += 1
        }
        query_end = o
    }
    var frag_start := -1
    if o < n {
        // The path and the query end only at '#' or the end.
        o += 1
        frag_start = o
        for o < n {
            if Encodes(s[o], InFragment) {
                return none
            }
            o += 1
        }
    }
    href := if upper { try LowerCopy(a, s, scheme_end, host_start, host_end) } else { try a.Dup(input) }
    value := if ip { Host.Ipv4(addr: addr) } else { Host.Domain(name: href[host_start..host_end]) }
    return some(Url(serial: href, scheme_end: scheme_end, user_start: host_start, user_end: host_start, pass_start: host_start, pass_end: host_start, host_start: host_start, host_end: host_end, host_value: some(value), port_value: port, path_start: path_start, path_end: path_end, query_start: query_start, query_end: query_end, frag_start: frag_start, opaque: false))
}

// LowerCopy is s, ASCII, as a str in a with the capitals of
// s[0..scheme_end] and s[host_start..host_end] lower-cased. O(n); allocates
// n bytes.
LowerCopy(a: &Arena, s: []u8, scheme_end: int, host_start: int, host_end: int): OutOfMemory!str from a {
    out := try a.Slice[u8](s.Len())
    (mut out[..]).CopyFrom(s)
    for k in 0..scheme_end {
        out[k] = Lower(out[k])
    }
    for k in host_start..host_end {
        out[k] = Lower(out[k])
    }
    // Must: out is ASCII, as s is.
    return rt.StrFrom(out[..]).Must()
}

// PathRunEnd is where PathRun's run from p ends in text, the offset of the
// '?' or '#' that ends it or text.Len(); or -1 - i when the byte at i
// refuses it: a byte the path set encodes, a '\', or the byte after a dot
// segment. O(n); allocates nothing.
PathRunEnd(text: []u8, p: int): int {
    var i := p
    var seg := i
    var dots := false
    for i < text.Len() {
        k := PathBytes[int(text[i])]
        if k == PathPlain {
            i += 1
            continue
        }
        if k == PathEnd {
            break
        }
        if k == PathRefuse {
            return -1 - i
        }
        if k == PathSlash {
            if dots && IsDotSegment(text[seg..i]) {
                return -1 - i
            }
            seg = i + 1
            dots = false
        } else {
            dots = true
        }
        i += 1
    }
    if dots && IsDotSegment(text[seg..i]) {
        return -1 - i
    }
    return i
}
