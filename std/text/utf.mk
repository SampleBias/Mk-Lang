// Why: every algorithm of std.text walks UTF-8 scalar by scalar, and
// Windows and JavaScript speak UTF-16, so both directions must be fast and
// exact.
//
// What: the scalar decoder and encoder std.text shares (Decode, Utf8Len),
// the table lookups over tables.mk (PropOf), and text.ToUtf16 and
// text.FromUtf16. Layer 1 of std.
//
// How: each conversion takes two passes, one to size the result exactly and
// one to write it, so it makes one allocation; FromUtf16 validates in the
// first pass, so an unpaired surrogate fails before anything is allocated.
// Both passes of both take 16 bytes or 8 to 16 units a vector where the text
// allows (std.simd at the 16-lane baseline; FromUtf16's write packs the 1-,
// 2- and 3-byte forms of four units with one pshufb, Utf16ToUtf8Vec;
// ToUtf16's write decodes runs of eight two-byte scalars a step, Utf8Two16,
// and any other mix of one- to three-byte scalars up to twelve bytes a step
// through one table lookup and one pshufb, std.simd.kernel's Utf8To16Step;
// ToUtf16's sizing is simd.Utf16Len, at the widest width Cpu() reports),
// and the scalar loops finish the rest. On x86_64-freestanding the vector
// steps are not compiled (each sits under a compile-time Target.os test, as
// std.simd's byte routines do), so a general-regs-only bin that calls
// ToUtf16 or FromUtf16 meets no E1108 here.

import std.raw
import std.simd
import std.simd.kernel

// Decode is the scalar at b[i] and its length in bytes; b is valid UTF-8
// and i the start of a scalar.
Decode(b: []u8, i: int): (c: u32, n: int) {
    lead := u32(b[i])
    if lead < 0x80 {
        return (c: lead, n: 1)
    }
    c1 := u32(b[i + 1]) & 0x3F
    if lead < 0xE0 {
        return (c: ((lead & 0x1F) << 6) | c1, n: 2)
    }
    c2 := u32(b[i + 2]) & 0x3F
    if lead < 0xF0 {
        return (c: ((lead & 0x0F) << 12) | (c1 << 6) | c2, n: 3)
    }
    c3 := u32(b[i + 3]) & 0x3F
    return (c: ((lead & 0x07) << 18) | (c1 << 12) | (c2 << 6) | c3, n: 4)
}

// Word is the eight bytes at b[i..i + 8], little-endian; LLVM folds the
// byte loads of a constant-length sub-slice into one load.
Word(b: []u8, i: int): u64 {
    w := b[i..][0..8]
    return u64(w[0]) | (u64(w[1]) << 8) | (u64(w[2]) << 16) | (u64(w[3]) << 24) | (u64(w[4]) << 32) | (u64(w[5]) << 40) | (u64(w[6]) << 48) | (u64(w[7]) << 56)
}

const HighBits: u64 = 0x8080808080808080

// Count8 is the number of bytes of m with bit 7 set; m has no other bit.
Count8(m: u64): int {
    return Trunc[int](((m >> 7) *% 0x0101010101010101) >> 56)
}

// Utf8Len is the length of scalar c in UTF-8.
Utf8Len(c: u32): int {
    if c < 0x80 {
        return 1
    }
    if c < 0x800 {
        return 2
    }
    if c < 0x10000 {
        return 3
    }
    return 4
}

// Encode writes scalar c (never a surrogate, at most 10FFFF) at out[o]
// and returns the offset after it.
Encode(out: mut []u8, o: int, c: u32): int {
    if c < 0x80 {
        out[o] = Trunc[u8](c)
        return o + 1
    }
    if c < 0x800 {
        out[o] = Trunc[u8](0xC0 | (c >> 6))
        out[o + 1] = Trunc[u8](0x80 | (c & 0x3F))
        return o + 2
    }
    if c < 0x10000 {
        out[o] = Trunc[u8](0xE0 | (c >> 12))
        out[o + 1] = Trunc[u8](0x80 | ((c >> 6) & 0x3F))
        out[o + 2] = Trunc[u8](0x80 | (c & 0x3F))
        return o + 3
    }
    out[o] = Trunc[u8](0xF0 | (c >> 18))
    out[o + 1] = Trunc[u8](0x80 | ((c >> 12) & 0x3F))
    out[o + 2] = Trunc[u8](0x80 | ((c >> 6) & 0x3F))
    out[o + 3] = Trunc[u8](0x80 | (c & 0x3F))
    return o + 4
}

// PropOf is the Prop byte of scalar c (tables.mk): one two-stage lookup.
PropOf(c: u32): u8 {
    idx := &PropIndex
    data := &PropData
    blk := u32(idx[c >> PropShift])
    return data[(blk << PropShift) | (c & ((u32(1) << PropShift) - 1))]
}

// NormId is the normalization record of scalar c, 0 for the trivial one.
NormId(c: u32): int {
    idx := &NormIndex
    data := &NormData
    blk := u32(idx[c >> NormShift])
    return int(data[(blk << NormShift) | (c & ((u32(1) << NormShift) - 1))])
}

// FoldOf is FoldData's entry for scalar c: offset << 2 | length into
// FoldPool, 0 when c folds to itself.
FoldOf(c: u32): u32 {
    idx := &FoldIndex
    data := &FoldData
    blk := u32(idx[c >> FoldShift])
    return u32(data[(blk << FoldShift) | (c & ((u32(1) << FoldShift) - 1))])
}

/// s as UTF-16 code units, with no byte order mark, in a. A scalar above
/// U+FFFF takes two units (a surrogate pair), any other one. Fails only
/// with OutOfMemory. O(n); one allocation in a of exactly the units
/// needed.
export ToUtf16(a: &Arena, s: str): OutOfMemory![]u16 from a {
    b := s.AsBytes()
    n := b.Len()
    // Units: every byte that is not a continuation (10xxxxxx), plus one
    // more for each four-byte lead (11110xxx): on a hosted target
    // simd.Utf16Len, whose vector forms run at the widest width Cpu()
    // reports; on freestanding eight bytes a step.
    var units := n
    var i := 0
    if Target.os != Os.Freestanding {
        units = simd.Utf16Len(s)
        i = n
    }
    for i + 8 <= n {
        w := Word(b, i)
        cont := w & ~(w << 1) & HighBits
        four := w & (w << 1) & (w << 2) & (w << 3) & HighBits
        units = units - Count8(cont) + Count8(four)
        i += 8
    }
    for x in b[i..] {
        if x & 0xC0 == 0x80 {
            units -= 1
        }
        if x >= 0xF0 {
            units += 1
        }
    }
    out := try a.Slice[u16](units)
    i = 0
    var o := 0
    for i < n {
        x := u32(b[i])
        if Target.os != Os.Freestanding && i + 16 <= n {
            v := simd.Vec[u8, 16].FromSlice(b, i)
            if (v >= simd.Vec[u8, 16].Splat(0x80)).Bits() == 0 {
                // Sixteen ASCII bytes widen to sixteen units in two
                // vectors (simdutf's ASCII fast path).
                v.WidenLo[u16, 8]().CopyTo(out, o)
                v.WidenHi[u16, 8]().CopyTo(out, o + 8)
                o += 16
                i += 16
                continue
            }
            if o + 8 <= out.Len() {
                // Sixteen bytes that are up to eight two-byte scalars
                // (Greek, Cyrillic, Hebrew, Arabic text) decode in one
                // step, entered only when the next two scalars are two-byte
                // too. Any other mix of one- to three-byte scalars decodes
                // up to twelve bytes a step through kernel.Utf8To16Step.
                if x & 0xE0 == 0xC0 && b[i + 2] & 0xE0 == 0xC0 && b[i + 4] & 0xE0 == 0xC0 {
                    if k := Utf8Two16(out, o, b, i) {
                        o += k
                        i += 2 * k
                        continue
                    }
                }
                st := kernel.Utf8To16Step(v)
                if st.read > 0 {
                    st.units.CopyTo(out, o)
                    o += st.wrote
                    i += st.read
                    continue
                }
                // A four-byte scalar first: it and the four-byte scalars
                // right after it go as surrogate pairs here, so a run of
                // them (emoji) pays one failed step, not one each. Any
                // other byte (only in invalid UTF-8, through raw.Str)
                // falls to the scalar path below.
                if x >= 0xF0 {
                    for i < n && b[i] >= 0xF0 {
                        q := b[i..i + 4]
                        c := (((u32(q[0]) & 0x07) << 18) | ((u32(q[1]) & 0x3F) << 12) | ((u32(q[2]) & 0x3F) << 6) | (u32(q[3]) & 0x3F)) - 0x10000
                        var d := mut out[o..o + 2]
                        d[0] = Trunc[u16](0xD800 + (c >> 10))
                        d[1] = Trunc[u16](0xDC00 + (c & 0x3FF))
                        o += 2
                        i += 4
                    }
                    continue
                }
            }
        }
        if x < 0x80 {
            if i + 8 <= n {
                w := Word(b, i)
                if w & HighBits == 0 {
                    // Eight ASCII bytes widen to eight units.
                    var d := mut out[o..o + 8]
                    for k in 0..8 {
                        d[k] = Trunc[u16](w >> (8 * k)) & 0xFF
                    }
                    o += 8
                    i += 8
                    continue
                }
            }
            out[o] = Trunc[u16](x)
            o += 1
            i += 1
            continue
        }
        if x < 0xE0 {
            out[o] = Trunc[u16](((x & 0x1F) << 6) | (u32(b[i + 1]) & 0x3F))
            o += 1
            i += 2
            continue
        }
        if x < 0xF0 {
            q := b[i..i + 3]
            out[o] = Trunc[u16](((x & 0x0F) << 12) | ((u32(q[1]) & 0x3F) << 6) | (u32(q[2]) & 0x3F))
            o += 1
            i += 3
            continue
        }
        q := b[i..i + 4]
        v := (((x & 0x07) << 18) | ((u32(q[1]) & 0x3F) << 12) | ((u32(q[2]) & 0x3F) << 6) | (u32(q[3]) & 0x3F)) - 0x10000
        var d := mut out[o..o + 2]
        d[0] = Trunc[u16](0xD800 + (v >> 10))
        d[1] = Trunc[u16](0xDC00 + (v & 0x3FF))
        o += 2
        i += 4
    }
    return out[..]
}

/// The text the UTF-16 code units spell, in a. Utf8Error on an unpaired
/// surrogate: a high one (D800 to DBFF) not followed by a low one (DC00 to
/// DFFF), or a low one not after a high one. That check runs before
/// anything is allocated. O(n); then one allocation in a of exactly the
/// bytes needed.
export FromUtf16(a: &Arena, units: []u16): Utf8Error|OutOfMemory!str from a {
    n := units.Len()
    var bytes := 0
    var i := 0
    if Target.os != Os.Freestanding && n >= 8 {
        // Eight units a vector (simdutf's utf8_length_from_utf16 and its
        // surrogate test): 1 byte each, one more from 0x80 and one more
        // from 0x800, for the units before the first surrogate; a
        // surrogate is checked as below, one pair at a time.
        f800 := simd.Vec[u16, 8].Splat(0xF800)
        d800 := simd.Vec[u16, 8].Splat(0xD800)
        x7f := simd.Vec[u16, 8].Splat(0x7F)
        x7ff := simd.Vec[u16, 8].Splat(0x7FF)
        for i + 8 <= n {
            v := simd.Vec[u16, 8].FromSlice(units, i)
            sur := ((v & f800) == d800).Bits()
            k := if sur != 0 { raw.Ctz64(sur) } else { 8 }
            low := (Cast[u64](1) << k) - 1
            bytes += k + raw.Popcount64((v > x7f).Bits() & low) + raw.Popcount64((v > x7ff).Bits() & low)
            i += k
            if sur != 0 {
                u := units[i]
                if u > 0xDBFF || i + 1 >= n || units[i + 1] < 0xDC00 || units[i + 1] > 0xDFFF {
                    fail Utf8Error
                }
                bytes += 4
                i += 2
            }
        }
    }
    for i < n {
        u := u32(units[i])
        if u & 0xF800 != 0xD800 {
            // 1, 2 or 3 bytes, without a branch on which.
            bytes += 1 + Trunc[int](((0x7F -% u) >> 31) + ((0x7FF -% u) >> 31))
            i += 1
            continue
        }
        if u <= 0xDBFF && i + 1 < n && units[i + 1] >= 0xDC00 && units[i + 1] <= 0xDFFF {
            bytes += 4
            i += 2
            continue
        }
        fail Utf8Error
    }
    out := try a.Slice[u8](bytes)
    var o := 0
    i = 0
    if Target.os != Os.Freestanding {
        oi := Utf16ToUtf8Vec(out, units)
        i = oi.i
        o = oi.o
    }
    for i < n {
        u := u32(units[i])
        if u < 0x80 {
            // The four-unit probe runs only from an ASCII unit. On a hosted
            // target this loop sees only the last 23 units or fewer, after
            // Utf16ToUtf8Vec.
            if i + 4 <= n {
                q := units[i..i + 4]
                if q[0] | q[1] | q[2] | q[3] < 0x80 {
                    var d := mut out[o..o + 4]
                    for k in 0..4 {
                        d[k] = Trunc[u8](q[k])
                    }
                    o += 4
                    i += 4
                    continue
                }
            }
            out[o] = Trunc[u8](u)
            o += 1
            i += 1
            continue
        }
        if u < 0x800 {
            var d := mut out[o..o + 2]
            d[0] = Trunc[u8](0xC0 | (u >> 6))
            d[1] = Trunc[u8](0x80 | (u & 0x3F))
            o += 2
            i += 1
            continue
        }
        if u & 0xF800 != 0xD800 {
            var d := mut out[o..o + 3]
            d[0] = Trunc[u8](0xE0 | (u >> 12))
            d[1] = Trunc[u8](0x80 | ((u >> 6) & 0x3F))
            d[2] = Trunc[u8](0x80 | (u & 0x3F))
            o += 3
            i += 1
            continue
        }
        // A high surrogate; pass 1 proved a low one follows.
        c := 0x10000 + ((u - 0xD800) << 10) + (u32(units[i + 1]) - 0xDC00)
        var d := mut out[o..o + 4]
        d[0] = Trunc[u8](0xF0 | (c >> 18))
        d[1] = Trunc[u8](0x80 | ((c >> 12) & 0x3F))
        d[2] = Trunc[u8](0x80 | ((c >> 6) & 0x3F))
        d[3] = Trunc[u8](0x80 | (c & 0x3F))
        o += 4
        i += 2
    }
    // The bytes are UTF-8 by construction; str.From checks them again, since
    // std.text has no unsafe. Not rt.StrFrom: std.text is layer 1,
    // so it builds for x86_64-freestanding, where std.rt is not (E0202).
    return try str.From(out[..])
}

// Utf8Two16 is ToUtf16's two-byte step: b[i] leads a
// two-byte scalar and i + 16 <= b.Len(). The sixteen bytes as eight u16
// lanes, lead in the low byte, are tested for (110xxxxx, 10xxxxxx); the
// lanes from the first that fails are not scalars of that shape (b is
// valid UTF-8 and the run starts at a scalar, so every lane before it is
// one). All eight lanes are decoded with two masks and a shift and stored
// at out[o], when out has room for eight; the first k are the k scalars
// read, and the units after them are rewritten by the steps that follow.
// So the result is the scalar loop's, unit for unit. none when no lane
// matches or out lacks the room; else some(k).
Utf8Two16(out: mut []u16, o: int, b: []u8, i: int): ?int {
    // The vector step is not compiled on freestanding (no E1108).
    if Target.os == Os.Freestanding || o + 8 > out.Len() {
        return none
    }
    w := simd.Vec[u8, 16].FromSlice(b, i).Reinterpret[u16, 8]()
    ok := ((w & simd.Vec[u16, 8].Splat(0xC0E0)) == simd.Vec[u16, 8].Splat(0x80C0)).Bits()
    k := raw.Ctz64(~ok)
    if k == 0 {
        return none
    }
    u := ((w & simd.Vec[u16, 8].Splat(0x1F)) << 6) | ((w >> 8) & simd.Vec[u16, 8].Splat(0x3F))
    u.CopyTo(out, o)
    return some(k)
}

// Utf8Pack[code] is the pshufb row that packs four units' UTF-8 forms,
// each spread over a 32-bit lane (lead byte first), into consecutive
// bytes: bit k of code is set when unit k takes two bytes or more, bit
// k + 4 when it takes three (simdutf's pack_1_2_3_utf8_bytes, built at
// compile time). An unused byte of a row is 0x80, which Lookup16 maps to
// 0. 4,096 bytes.
const Utf8Pack: [256][16]u8 = Utf8PackRows()

Utf8PackRows(): [256][16]u8 {
    var t: [256][16]u8 = [[0x80; 16]; 256]
    for code in 0..256 {
        var at := 0
        for lane in 0..4 {
            len := 1 + ((code >> lane) & 1) + ((code >> (lane + 4)) & 1)
            for b in 0..len {
                t[code][at] = Trunc[u8](4 * lane + b)
                at += 1
            }
        }
    }
    return t
}

// Utf8Lanes is the UTF-8 form of each of four units below 0xD800 or from
// 0xE000, spread over its 32-bit lane, lead byte first: one byte below
// 0x80, two below 0x800, else three.
Utf8Lanes(u: simd.Vec[u32, 4]): simd.Vec[u8, 16] {
    x3f := simd.Vec[u32, 4].Splat(0x3F)
    x80 := simd.Vec[u32, 4].Splat(0x80)
    lo := ((u & x3f) | x80)
    mid := (((u >> 6) & x3f) | x80)
    two := ((u >> 6) | simd.Vec[u32, 4].Splat(0xC0)) | (lo << 8)
    three := ((u >> 12) | simd.Vec[u32, 4].Splat(0xE0)) | (mid << 8) | (lo << 16)
    w := (u > simd.Vec[u32, 4].Splat(0x7FF)).Select(three, (u > simd.Vec[u32, 4].Splat(0x7F)).Select(two, u))
    return w.Reinterpret[u8, 16]()
}

// Utf16ToUtf8Vec is FromUtf16's write pass while 24 units remain, which
// keeps every 16-byte store inside out (each unit left takes at least one
// byte): sixteen ASCII units narrow to sixteen bytes in one saturating
// pack; else the units before the first surrogate of the next eight
// are narrowed in one pack when they are ASCII, or else, four at a time,
// spread to their UTF-8 forms in 32-bit lanes and packed by one pshufb
// through Utf8Pack (simdutf's utf16_to_utf8 for the 1-, 2- and 3-byte
// units); then that surrogate's pair, which pass 1 proved paired, is
// written as four bytes in the same step. It returns the units
// read and the bytes written; the scalar loop finishes the rest. Hosted
// targets only (16-lane vectors, pshufb at the x86-64-v2 baseline).
Utf16ToUtf8Vec(out: mut []u8, units: []u16): (i: int, o: int) {
    n := units.Len()
    x7f := simd.Vec[u16, 8].Splat(0x7F)
    x7ff := simd.Vec[u16, 8].Splat(0x7FF)
    f800 := simd.Vec[u16, 8].Splat(0xF800)
    d800 := simd.Vec[u16, 8].Splat(0xD800)
    pack := &Utf8Pack
    var i := 0
    var o := 0
    for i + 24 <= n {
        va := simd.Vec[u16, 8].FromSlice(units, i)
        vb := simd.Vec[u16, 8].FromSlice(units, i + 8)
        if ((va | vb) > x7f).Bits() == 0 {
            va.NarrowSat[u8, 16](vb).CopyTo(out, o)
            o += 16
            i += 16
            continue
        }
        // k units, 0 to 8, come before the first surrogate.
        sur := ((va & f800) == d800).Bits()
        k := if sur != 0 { raw.Ctz64(sur) } else { 8 }
        keep := (Cast[u64](1) << k) - 1
        m1 := (va > x7f).Bits() & keep
        if k == 0 {
            // Pairs first: written one after another, with no pack.
            for i + 24 <= n && units[i] & 0xF800 == 0xD800 {
                c := 0x10000 + ((u32(units[i]) - 0xD800) << 10) + (u32(units[i + 1]) - 0xDC00)
                var d := mut out[o..o + 4]
                d[0] = Trunc[u8](0xF0 | (c >> 18))
                d[1] = Trunc[u8](0x80 | ((c >> 12) & 0x3F))
                d[2] = Trunc[u8](0x80 | ((c >> 6) & 0x3F))
                d[3] = Trunc[u8](0x80 | (c & 0x3F))
                o += 4
                i += 2
            }
            continue
        } else if m1 == 0 {
            // The k units are ASCII: one narrowing pack, of which the
            // first k bytes count.
            va.NarrowSat[u8, 16](vb).CopyTo(out, o)
            o += k
        } else {
            // Lanes past the k units are packed too, after their bytes,
            // and overwritten by the next write.
            m2 := (va > x7ff).Bits() & keep
            all1 := (va > x7f).Bits()
            all2 := (va > x7ff).Bits()
            k0 := if k < 4 { k } else { 4 }
            row0 := simd.Vec[u8, 16].FromArray(pack[Cast[int]((all1 & 15) | ((all2 & 15) << 4))])
            simd.Lookup16[16](row0, Utf8Lanes(va.WidenLo[u32, 4]())).CopyTo(out, o)
            o += k0 + raw.Popcount64(m1 & 15) + raw.Popcount64(m2 & 15)
            if k > 4 {
                row1 := simd.Vec[u8, 16].FromArray(pack[Cast[int](((all1 >> 4) & 15) | (((all2 >> 4) & 15) << 4))])
                simd.Lookup16[16](row1, Utf8Lanes(va.WidenHi[u32, 4]())).CopyTo(out, o)
                o += k - 4 + raw.Popcount64(m1 >> 4) + raw.Popcount64(m2 >> 4)
            }
        }
        i += k
        if k < 8 {
            // A high surrogate at lane k; pass 1 proved a low one follows.
            c := 0x10000 + ((u32(units[i]) - 0xD800) << 10) + (u32(units[i + 1]) - 0xDC00)
            var d := mut out[o..o + 4]
            d[0] = Trunc[u8](0xF0 | (c >> 18))
            d[1] = Trunc[u8](0x80 | ((c >> 12) & 0x3F))
            d[2] = Trunc[u8](0x80 | ((c >> 6) & 0x3F))
            d[3] = Trunc[u8](0x80 | (c & 0x3F))
            o += 4
            i += 2
        }
    }
    return (i: i, o: o)
}
