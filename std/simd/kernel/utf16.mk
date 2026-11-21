// Why: UTF-8 to UTF-16 conversion (text.ToUtf16, encoding's UTF-16LE and BE
// encoders) is a scalar at a time on mixed-script text unless one vector
// step takes any mix of scalar lengths; both callers need the same step and
// its 26 KiB of tables, so they live here once.
//
// What: Utf8To16Step, the step, with its tables StepIndex and StepShuf.
//
// How: simdutf's convert_masked_utf8_to_utf16. The 12-bit mask of the bytes
// of a 16-byte load that end a scalar (those followed by a byte that is not
// a continuation) indexes StepIndex, which names a pshufb row of StepShuf
// and how many bytes and units the step takes. Up to eight scalars of one or
// two bytes go to 16-bit lanes, last byte low and lead high; else up to four
// of up to three bytes go to 32-bit lanes, last byte first, packed to 16
// bits after decoding. Masks and shifts decode every lane at once. Both
// tables are built at compile time.

import std.simd

/// The step over v, sixteen bytes from the start of a scalar: how many bytes
/// it read, how many UTF-16 units it made, and the units in the low lanes of
/// units (the lanes past wrote are garbage; a caller stores all eight and
/// lets its next step overwrite them). read is 0 when the first scalar is
/// not one of one to three bytes (a four-byte scalar), and then the caller
/// takes it a scalar at a time. On valid UTF-8 the units are exact; on
/// other bytes they are garbage, but read is never more than 12. O(1); a
/// table load and one pshufb; allocates nothing. Hosted targets: the caller
/// keeps it off x86_64-freestanding.
export Utf8To16Step(v: simd.Vec[u8, 16]): (read: int, wrote: int, units: simd.Vec[u16, 8]) {
    lead := ((v & simd.Vec[u8, 16].Splat(0xC0)) != simd.Vec[u8, 16].Splat(0x80)).Bits()
    e := StepIndex[Cast[int]((lead >> 1) & 0xFFF)]
    if e == 0 {
        return (read: 0, wrote: 0, units: simd.Vec[u16, 8].Zero())
    }
    id := Cast[int](e & 0x3FF)
    p := simd.Lookup16[16](simd.Vec[u8, 16].FromSlice(StepShuf[..], id * 16), v)
    var u := simd.Vec[u16, 8].Zero()
    if id < StepWide {
        w := p.Reinterpret[u16, 8]()
        u = (w & simd.Vec[u16, 8].Splat(0x7F)) | ((w & simd.Vec[u16, 8].Splat(0x1F00)) >> 2)
    } else {
        w := p.Reinterpret[u32, 4]()
        d := (w & simd.Vec[u32, 4].Splat(0x7F)) | ((w & simd.Vec[u32, 4].Splat(0x3F00)) >> 2) | ((w & simd.Vec[u32, 4].Splat(0x0F0000)) >> 4)
        // The low two bytes of each 32-bit lane, packed to the front.
        u = d.Reinterpret[u8, 16]().Shuffle(simd.Vec[u8, 16].Zero(), [0, 1, 4, 5, 8, 9, 12, 13, 16, 16, 16, 16, 16, 16, 16, 16]).Reinterpret[u16, 8]()
    }
    return (read: Cast[int]((e >> 10) & 0xF), wrote: Cast[int](e >> 14), units: u)
}

// StepIndex[mask] is Utf8To16Step's step for a 12-bit mask of the bytes
// that end a scalar: the StepShuf row in bits 0 to 9, the bytes read in
// bits 10 to 13, the units written from bit 14; 0 when the first scalar
// has four bytes. Of the scalars the mask ends, the step takes the longer
// in bytes of two prefixes, the 16-bit form on a tie: the leading ones of
// one or two bytes, eight at most, and the leading ones of up to three
// bytes, four at most. 16 KiB, built at compile time.
const StepIndex: [4096]u32 = StepIndexRows()

// StepShuf is Utf8To16Step's pshufb rows, 16 bytes each, 0x80 for a zero
// byte. Rows below StepWide are the 16-bit form of k scalars of one or two
// bytes, at 2^k - 2 plus a k-bit number whose bit t is set when scalar t
// has two bytes (k from 1 to 8); from StepWide, the 32-bit form of k
// scalars of one to three bytes, at StepWide + (3^k - 3) / 2 plus a base-3
// number whose digit t is scalar t's length less one (k from 1 to 4). 630
// rows, 10,080 bytes, built at compile time.
const StepShuf: [10080]u8 = StepShufRows()
const StepWide: int = 510

StepShufRows(): [10080]u8 {
    var t: [10080]u8 = [0x80; 10080]
    for k in 1..9 {
        for bits in 0..(1 << k) {
            row := ((1 << k) - 2 + bits) * 16
            var s := 0
            for j in 0..k {
                z := 1 + ((bits >> j) & 1)
                if s + z <= 16 {
                    t[row + 2 * j] = Trunc[u8](s + z - 1)
                    if z == 2 {
                        t[row + 2 * j + 1] = Trunc[u8](s)
                    }
                }
                s += z
            }
        }
    }
    var p := 1
    for k in 1..5 {
        for digits in 0..3 * p {
            row := (StepWide + (3 * p - 3) / 2 + digits) * 16
            var s := 0
            var d := digits
            for j in 0..k {
                z := 1 + d % 3
                d = d / 3
                t[row + 4 * j] = Trunc[u8](s + z - 1)
                if z >= 2 {
                    t[row + 4 * j + 1] = Trunc[u8](s + z - 2)
                }
                if z == 3 {
                    t[row + 4 * j + 2] = Trunc[u8](s)
                }
                s += z
            }
        }
        p = 3 * p
    }
    return t
}

StepIndexRows(): [4096]u32 {
    var t: [4096]u32 = [0; 4096]
    for mask in 0..4096 {
        // The two prefixes: the count, bytes and row offset of each.
        var ka := 0
        var ba := 0
        var ra := 0
        var kb := 0
        var bb := 0
        var rb := 0
        var pb := 1
        var a_open := true
        var b_open := true
        var start := 0
        for j in 0..12 {
            if (mask >> j) & 1 != 0 {
                z := j - start + 1
                start = j + 1
                if a_open && z <= 2 && ka < 8 {
                    if z == 2 {
                        ra = ra | (1 << ka)
                    }
                    ka += 1
                    ba += z
                } else {
                    a_open = false
                }
                if b_open && z <= 3 && kb < 4 {
                    rb += (z - 1) * pb
                    pb = 3 * pb
                    kb += 1
                    bb += z
                } else {
                    b_open = false
                }
            }
        }
        if ka > 0 && ba >= bb {
            t[mask] = Trunc[u32](((1 << ka) - 2 + ra) | (ba << 10) | (ka << 14))
        } else if kb > 0 {
            t[mask] = Trunc[u32]((StepWide + (pb - 3) / 2 + rb) | (bb << 10) | (kb << 14))
        }
    }
    return t
}
