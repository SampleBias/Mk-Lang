// Why: UTF-16 text (Windows, JavaScript, Java) is mostly runs of
// non-surrogate units, which can be converted to UTF-8 many at a time.
//
// What: Utf16Run, the UTF-16 half of text.mk's Run, with Utf16Units,
// Utf16Write, Utf8Words and the Pack shuffle tables.
//
// How: the WHATWG shared UTF-16 decoder (Utf16Step) behind a fast path.
// Eight units a step that hold no surrogate are counted, or written, as a
// vector: each is one UTF-8 byte below 0x80, two below 0x800 and three
// otherwise, so a count is two mask counts; eight ASCII units narrow to
// eight bytes in one store, and other blocks, while 32 bytes of output
// remain, become UTF-8 words gathered by one shuffle per four units
// (Pack). That is simdutf's utf16_to_utf8 at the 16-byte baseline. Only
// surrogates and the end of the input go through Utf16Step, so malformed
// input decodes exactly as the standard's decoder does. The other direction,
// Utf16Write, takes sixteen ASCII bytes or any mix of one- to three-byte
// scalars a vector step (std.simd.kernel's Utf8To16Step: one table lookup and
// one pshufb).

import std.simd
import std.simd.kernel

// Swap16 swaps the two bytes of every 16-bit lane, for UTF-16BE.
Swap16(v: simd.Vec[u8, 16]): simd.Vec[u8, 16] {
    return v.Shuffle(v, [1, 0, 3, 2, 5, 4, 7, 6, 9, 8, 11, 10, 13, 12, 15, 14])
}

// Utf16Run is Run for UTF-16LE (be false) and UTF-16BE.
Utf16Run(b: []u8, stop: int, last: bool, be: bool, out: mut []u8, write: bool, strict: bool): Ran {
    var i := 0
    var o := 0
    var bad := -1
    surrogate_mask := simd.Vec[u16, 8].Splat(0xF800)
    surrogate := simd.Vec[u16, 8].Splat(0xD800)
    x80 := simd.Vec[u16, 8].Splat(0x80)
    x800 := simd.Vec[u16, 8].Splat(0x800)
    for i < stop {
        if stop - i >= 16 {
            raw := simd.Vec[u8, 16].FromSlice(b, i)
            ordered := if be { Swap16(raw) } else { raw }
            w := ordered.Reinterpret[u16, 8]()
            if !((w & surrogate_mask) == surrogate).Any() {
                wide := (w >= x80).CountSet()
                if !write {
                    o += 8 + wide + (w >= x800).CountSet()
                    i += 16
                    continue
                }
                if wide == 0 {
                    w.Trunc[u8]().CopyTo(out, o)
                    o += 8
                    i += 16
                    continue
                }
                if out.Len() - o >= 32 {
                    // No surrogate: each half's four scalars as UTF-8 in
                    // 32-bit words, gathered by one shuffle that its
                    // masks pick. Each store writes 16 bytes for at most
                    // 12, and the second starts at most 12 past the first.
                    two := (w >= x80).Bits()
                    three := (w >= x800).Bits()
                    lo := Trunc[int]((two & 0xF) | ((three & 0xF) << 4))
                    hi := Trunc[int]((two >> 4) | (three & 0xF0))
                    Pack(Utf8Words(w.WidenLo[u32, 4]()), lo).CopyTo(out, o)
                    o += int(PackLen[lo])
                    Pack(Utf8Words(w.WidenHi[u32, 4]()), hi).CopyTo(out, o)
                    o += int(PackLen[hi])
                    i += 16
                    continue
                }
                // Near the end of out: one scalar at a time.
                units := w.ToArray()
                for k in 0..8 {
                    o = PutBmp(out, o, u32(units[k]))
                }
                i += 16
                continue
            }
        }
        u := Unit16Or(b, i, be)
        if u < 0x80 {
            if write {
                out[o] = Trunc[u8](u)
            }
            o += 1
            i += 2
            continue
        }
        if u < 0x800 {
            if write {
                var d := mut out[o..o + 2]
                d[0] = Trunc[u8](0xC0 | (u >> 6))
                d[1] = Trunc[u8](0x80 | (u & 0x3F))
            }
            o += 2
            i += 2
            continue
        }
        if u < 0xD800 || u > 0xDFFF && u <= 0xFFFF {
            if write {
                var d := mut out[o..o + 3]
                d[0] = Trunc[u8](0xE0 | (u >> 12))
                d[1] = Trunc[u8](0x80 | ((u >> 6) & 0x3F))
                d[2] = Trunc[u8](0x80 | (u & 0x3F))
            }
            o += 3
            i += 2
            continue
        }
        st := Utf16Step(b, i, last, be)
        if st.n == 0 {
            break
        }
        if st.c == Bad {
            if bad < 0 {
                bad = i
            }
            if strict {
                break
            }
        }
        o = if write { Put(out, o, st.c) } else { o + Utf8Len(st.c) }
        i += st.n
    }
    return (read: i, wrote: o, bad)
}

// Utf8Words is each lane of u, a scalar that is no surrogate, as UTF-8 in
// a 32-bit word, first byte lowest.
Utf8Words(u: simd.Vec[u32, 4]): simd.Vec[u32, 4] {
    six := simd.Vec[u32, 4].Splat(0x3F)
    cont := simd.Vec[u32, 4].Splat(0x80)
    low := (u & six) | cont
    mid := ((u >> 6) & six) | cont
    two := (u >> 6) | simd.Vec[u32, 4].Splat(0xC0) | (low << 8)
    three := (u >> 12) | simd.Vec[u32, 4].Splat(0xE0) | (mid << 8) | (low << 16)
    return (u < cont).Select(u, (u < simd.Vec[u32, 4].Splat(0x800)).Select(two, three))
}

// Pack gathers the UTF-8 bytes of words, as Utf8Words made them, to the
// front of 16 bytes; m's low four bits are the lanes of two bytes or more,
// its high four the lanes of three. The rest of the bytes are 0.
Pack(words: simd.Vec[u32, 4], m: int): simd.Vec[u8, 16] {
    return simd.Lookup16[16](simd.Vec[u8, 16].FromSlice(PackShuf[..], m * 16), words.Reinterpret[u8, 16]())
}

// PackShuf is, for each m of Pack, the lane each byte is taken from (16 for
// none, which reads as 0); PackLen is how many bytes that is. An m with a
// lane of three bytes that is not one of two never occurs. The method of
// simdutf's utf16_to_utf8 (its pack_1_2_3_utf8_bytes table), built at
// compile time: 4,096 and 256 bytes.
const PackShuf: [4096]u8 = PackShuffles()
const PackLen: [256]u8 = PackLengths()

PackShuffles(): [4096]u8 {
    var t: [4096]u8 = [16; 4096]
    for m in 0..256 {
        var p := 0
        for j in 0..4 {
            n := 1 + ((m >> j) & 1) + ((m >> (j + 4)) & 1)
            for k in 0..n {
                t[m * 16 + p] = Trunc[u8](4 * j + k)
                p += 1
            }
        }
    }
    return t
}

PackLengths(): [256]u8 {
    var t: [256]u8 = [0; 256]
    for m in 0..256 {
        var p := 0
        for j in 0..4 {
            p += 1 + ((m >> j) & 1) + ((m >> (j + 4)) & 1)
        }
        t[m] = Trunc[u8](p)
    }
    return t
}

// Unit16Or is the unit at b[i], or 0x10000 (no unit, so Utf16Step decides)
// when fewer than two bytes remain.
Unit16Or(b: []u8, i: int, be: bool): u32 {
    if b.Len() - i < 2 {
        return 0x10000
    }
    return Unit16(b, i, be)
}

// PutBmp writes u, a unit that is no surrogate, as UTF-8 at out[o] and
// returns the offset after it.
PutBmp(out: mut []u8, o: int, u: u32): int {
    if u < 0x80 {
        out[o] = Trunc[u8](u)
        return o + 1
    }
    if u < 0x800 {
        var d := mut out[o..o + 2]
        d[0] = Trunc[u8](0xC0 | (u >> 6))
        d[1] = Trunc[u8](0x80 | (u & 0x3F))
        return o + 2
    }
    var d := mut out[o..o + 3]
    d[0] = Trunc[u8](0xE0 | (u >> 12))
    d[1] = Trunc[u8](0x80 | ((u >> 6) & 0x3F))
    d[2] = Trunc[u8](0x80 | (u & 0x3F))
    return o + 3
}

// Utf16Units is how many UTF-16 units valid UTF-8 b encodes to: one per
// byte that is not a continuation byte, and one more per four-byte lead.
// Sixteen bytes a step by two mask counts.
Utf16Units(b: []u8): int {
    n := b.Len()
    top := simd.Vec[u8, 16].Splat(0xC0)
    cont := simd.Vec[u8, 16].Splat(0x80)
    four := simd.Vec[u8, 16].Splat(0xF0)
    var units := 0
    var i := 0
    for n - i >= 16 {
        v := simd.Vec[u8, 16].FromSlice(b, i)
        units += 16 - ((v & top) == cont).CountSet() + (v >= four).CountSet()
        i += 16
    }
    for x in b[i..] {
        if x & 0xC0 != 0x80 {
            units += 1
        }
        if x >= 0xF0 {
            units += 1
        }
    }
    return units
}

// Utf16Write writes valid UTF-8 b as UTF-16 units to out, which holds
// exactly Utf16Units(b) units, in the byte order be says. Sixteen ASCII
// bytes a step widen to sixteen units in two stores (the ASCII path of
// simdutf's utf8_to_utf16); other sixteen-byte blocks, while 16 bytes of
// output remain, go through std.simd.kernel's Utf8To16Step, up to twelve bytes
// a step; a four-byte scalar and the last 15 bytes are written a scalar at a
// time.
Utf16Write(b: []u8, out: mut []u8, be: bool) {
    n := b.Len()
    high := simd.Vec[u8, 16].Splat(0x80)
    zero := simd.Vec[u8, 16].Zero()
    var i := 0
    var o := 0
    for i < n {
        if n - i >= 16 {
            v := simd.Vec[u8, 16].FromSlice(b, i)
            if !(v >= high).Any() {
                if be {
                    zero.InterleaveLo(v).CopyTo(out, o)
                    zero.InterleaveHi(v).CopyTo(out, o + 16)
                } else {
                    v.InterleaveLo(zero).CopyTo(out, o)
                    v.InterleaveHi(zero).CopyTo(out, o + 16)
                }
                i += 16
                o += 32
                continue
            }
            if out.Len() - o >= 16 {
                st := kernel.Utf8To16Step(v)
                if st.read > 0 {
                    u := st.units.Reinterpret[u8, 16]()
                    if be {
                        Swap16(u).CopyTo(out, o)
                    } else {
                        u.CopyTo(out, o)
                    }
                    i += st.read
                    o += 2 * st.wrote
                    continue
                }
                // A four-byte scalar first: it and the four-byte scalars
                // right after it go as surrogate pairs here, so a run of
                // them (emoji) pays one failed step, not one each. Any
                // other byte (only in invalid UTF-8, through raw.Str)
                // falls to the scalar path below.
                if b[i] >= 0xF0 {
                    for i < n && b[i] >= 0xF0 {
                        c := ScalarAt(b, i).c - 0x10000
                        PutUnit(out, o, 0xD800 | (c >> 10), be)
                        PutUnit(out, o + 2, 0xDC00 | (c & 0x3FF), be)
                        o += 4
                        i += 4
                    }
                    continue
                }
            }
        }
        lead := u32(b[i])
        if lead < 0x80 {
            PutUnit(out, o, lead, be)
            o += 2
            i += 1
            continue
        }
        sc := ScalarAt(b, i)
        if sc.c < 0x10000 {
            PutUnit(out, o, sc.c, be)
            o += 2
        } else {
            v := sc.c - 0x10000
            PutUnit(out, o, 0xD800 | (v >> 10), be)
            PutUnit(out, o + 2, 0xDC00 | (v & 0x3FF), be)
            o += 4
        }
        i += sc.n
    }
}
