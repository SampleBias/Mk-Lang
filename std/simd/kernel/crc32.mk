// Why: CRC-32 checks every zip, gzip and PNG stream, and a table loop is far
// slower than folding with the CPU's carry-less multiply.
//
// What: Crc32Fold, the CRC-32 folding kernel behind its simd.Cpu() guard,
// with its 128-bit body Crc32Ni and its AVX-512 body Crc32Wide, and
// CrcFoldK, its folding constants.
//
// How: the folding of Gopal et al., "Fast CRC Computation for Generic
// Polynomials Using PCLMULQDQ Instruction" (Intel, 2009), for
// CRC-32/ISO-HDLC's reflected polynomial 0xEDB88320, in the shape of Go's
// ieeeCLMUL and Rust crc32fast's pclmulqdq.rs: four 128-bit accumulators
// folded 64 bytes at a time, then into one, then 16 bytes at a time.
//
// A 16-byte block is held as Vec[u64, 2], lane 0 the little-endian word of
// bytes 0 to 7. Folding a block a over a distance of d bits onto b is
// b ^ clmul(a.lo, k(d + 32)) ^ clmul(a.hi, k(d - 32)), with k(n) the
// reflected x^n mod P (CrcFoldK). Every fold keeps the invariant that the
// CRC register from zero over the accumulator's 16 bytes equals the
// register c over the bytes consumed so far. So the kernel stops there and
// hands the 16 bytes back: std.hash runs its table over them, and over the
// tail, and needs neither the paper's 64-bit step nor its Barrett
// reduction. Each cpu clause names sse2 too: PCLMULQDQ works in xmm
// registers (E1106 needs on x86_64; aarch64 ignores the name).
//
// On x86_64 with avx512f and vpclmulqdq, inputs of 256 bytes or more take
// Crc32Wide, Go's AVX-512 shape: the same folds on 64-byte zmm
// accumulators, each holding four blocks, 256 bytes per step (CrcZ1 and
// CrcZ2 fold over 2,048 bits), then into one 64-byte accumulator whose four
// blocks are the four accumulators of the 128-bit loop (CrcFour). Its
// carry-less multiplies are asm statements, since raw.Clmul64 has no vector
// form; the path is behind a compile-time arch test, so no other target
// compiles them.

import std.raw
import std.simd

// CrcFoldK is x^n mod P(x) for CRC-32's polynomial P = 0x104C11DB7,
// bit-reflected in 32 bits and shifted left by one: Gopal et al.'s
// constant for a fold whose clmul term ends n bits ahead. A const
// initializer calls it, so it runs only in the evaluator.
CrcFoldK(n: int): u64 {
    var r: u64 = 1
    for _ in 0..n {
        r = r << 1
        if r & 0x100000000 != 0 {
            r = r ^ 0x104C11DB7
        }
    }
    var f: u64 = 0
    for b in 0..32 {
        if (r >> b) & 1 != 0 {
            f = f | (u64(1) << (31 - b))
        }
    }
    return f << 1
}

// CrcK1 and CrcK2 fold a block onto the one 64 bytes on (the four-way
// loop); CrcK3 and CrcK4 fold it onto the next block. They are
// 0x154442bd4, 0x1c6e41596, 0x1751997d0 and 0x0ccaa009e, crc32fast's K1 to
// K4.
const CrcK1: u64 = CrcFoldK(4 * 128 + 32)
const CrcK2: u64 = CrcFoldK(4 * 128 - 32)
const CrcK3: u64 = CrcFoldK(128 + 32)
const CrcK4: u64 = CrcFoldK(128 - 32)

// CrcBlock is the 16 bytes of data from i, one bounds check.
CrcBlock(data: []u8, i: int): simd.Vec[u64, 2] {
    return simd.Vec[u8, 16].FromSlice(data, i).Reinterpret[u64, 2]()
}

// CrcFold is a folded onto b with the constants k1 (for a's low word) and
// k2 (its high word).
CrcFold(a: simd.Vec[u64, 2], b: simd.Vec[u64, 2], k1: u64, k2: u64): simd.Vec[u64, 2] cpu "sse2, pclmul, aes" {
    p := raw.Clmul64(a.Lane(0), k1)
    q := raw.Clmul64(a.Lane(1), k2)
    return b ^ simd.Vec[u64, 2].FromArray([p.lo ^ q.lo, p.hi ^ q.hi])
}

// Crc32Ni is Crc32Fold's body, for data.Len() >= 64, without AVX-512: one
// function, so the four accumulators stay in registers.
Crc32Ni(c: u32, data: []u8, fold: mut [16]u8): int cpu "sse2, pclmul, aes" {
    n := data.Len() / 16 * 16
    var x3 := CrcBlock(data, 0) ^ simd.Vec[u64, 2].FromArray([u64(c), 0])
    var x2 := CrcBlock(data, 16)
    var x1 := CrcBlock(data, 32)
    var x0 := CrcBlock(data, 48)
    var i := 64
    for n - i >= 64 {
        x3 = CrcFold(x3, CrcBlock(data, i), CrcK1, CrcK2)
        x2 = CrcFold(x2, CrcBlock(data, i + 16), CrcK1, CrcK2)
        x1 = CrcFold(x1, CrcBlock(data, i + 32), CrcK1, CrcK2)
        x0 = CrcFold(x0, CrcBlock(data, i + 48), CrcK1, CrcK2)
        i += 64
    }
    var x := CrcFold(CrcFold(CrcFold(x3, x2, CrcK3, CrcK4), x1, CrcK3, CrcK4), x0, CrcK3, CrcK4)
    for n - i >= 16 {
        x = CrcFold(x, CrcBlock(data, i), CrcK3, CrcK4)
        i += 16
    }
    *fold = x.Reinterpret[u8, 16]().ToArray()
    return n
}

// CrcFour is Crc32Wide's 128-bit tail, Crc32Ni's loops from at on: it
// folds data from at on, given the four accumulators x3 to x0 of the 64
// bytes before at, 64 bytes at a time, then into one, then 16 bytes
// at a time; it writes the last accumulator to fold and returns the bytes
// consumed, data.Len() / 16 * 16.
CrcFour(a3: simd.Vec[u64, 2], a2: simd.Vec[u64, 2], a1: simd.Vec[u64, 2], a0: simd.Vec[u64, 2], data: []u8, at: int, fold: mut [16]u8): int cpu "sse2, pclmul, aes" {
    n := data.Len() / 16 * 16
    var x3 := a3
    var x2 := a2
    var x1 := a1
    var x0 := a0
    var i := at
    for n - i >= 64 {
        x3 = CrcFold(x3, CrcBlock(data, i), CrcK1, CrcK2)
        x2 = CrcFold(x2, CrcBlock(data, i + 16), CrcK1, CrcK2)
        x1 = CrcFold(x1, CrcBlock(data, i + 32), CrcK1, CrcK2)
        x0 = CrcFold(x0, CrcBlock(data, i + 48), CrcK1, CrcK2)
        i += 64
    }
    var x := CrcFold(CrcFold(CrcFold(x3, x2, CrcK3, CrcK4), x1, CrcK3, CrcK4), x0, CrcK3, CrcK4)
    for n - i >= 16 {
        x = CrcFold(x, CrcBlock(data, i), CrcK3, CrcK4)
        i += 16
    }
    *fold = x.Reinterpret[u8, 16]().ToArray()
    return n
}

// CrcZ1 and CrcZ2 fold each 16-byte lane of a 64-byte block onto the lane
// 256 bytes on (the four-way loop of Crc32Wide, whose four accumulators are
// 64 bytes each); CrcK1 and CrcK2 then fold a 64-byte block onto the next.
const CrcZ1: u64 = CrcFoldK(16 * 128 + 32)
const CrcZ2: u64 = CrcFoldK(16 * 128 - 32)

// CrcWide is the 64 bytes of data from i, one bounds check.
CrcWide(data: []u8, i: int): simd.Vec[u64, 8] cpu "sse2, pclmul, aes, avx512f, vpclmulqdq" {
    return simd.Vec[u8, 64].FromSlice(data, i).Reinterpret[u64, 8]()
}

// Crc32Wide is Crc32Fold's body on x86_64 with AVX-512 and VPCLMULQDQ, for
// data.Len() >= 256: four 64-byte accumulators, each four 16-byte lanes
// folded 256 bytes at a time, then into one; its four lanes are the four
// accumulators CrcFour takes over, as Go's AVX-512 path hands its zmm
// accumulators to the 128-bit tail.
//
// A fold of x onto d is CrcFold on each 16-byte lane, with k holding
// (k1, k2) in every lane: VPCLMULQDQ multiplies the four low words by k1
// (selector 0x00) and the four high words by k2 (0x11), and VPTERNLOGQ
// 0x96 xors both products into d. raw.Clmul64 has no vector form, so each
// group of folds is one asm statement on registers only, written out in
// this function: an asm-holding helper is not inlined into it, and its
// call would pass every zmm value through the stack.
Crc32Wide(c: u32, data: []u8, fold: mut [16]u8): int cpu "sse2, pclmul, aes, avx512f, vpclmulqdq" {
    n := data.Len()
    kz := simd.Vec[u64, 8].FromArray([CrcZ1, CrcZ2, CrcZ1, CrcZ2, CrcZ1, CrcZ2, CrcZ1, CrcZ2])
    k4 := simd.Vec[u64, 8].FromArray([CrcK1, CrcK2, CrcK1, CrcK2, CrcK1, CrcK2, CrcK1, CrcK2])
    var x3 := CrcWide(data, 0) ^ simd.Vec[u64, 8].FromArray([u64(c), 0, 0, 0, 0, 0, 0, 0])
    var x2 := CrcWide(data, 64)
    var x1 := CrcWide(data, 128)
    var x0 := CrcWide(data, 192)
    var t3 := x3
    var t2 := x2
    var t1 := x1
    var t0 := x0
    var i := 256
    for n - i >= 256 {
        w := data[i..i + 256]
        d3 := CrcWide(w, 0)
        d2 := CrcWide(w, 64)
        d1 := CrcWide(w, 128)
        d0 := CrcWide(w, 192)
        unsafe "vpclmulqdq and vpternlogq read and write only these zmm registers; the clause names avx512f and vpclmulqdq" {
            asm("vpclmulqdq {t3}, {x3}, {k}, 0x00\nvpclmulqdq {t2}, {x2}, {k}, 0x00\nvpclmulqdq {t1}, {x1}, {k}, 0x00\nvpclmulqdq {t0}, {x0}, {k}, 0x00\nvpclmulqdq {x3}, {x3}, {k}, 0x11\nvpclmulqdq {x2}, {x2}, {k}, 0x11\nvpclmulqdq {x1}, {x1}, {k}, 0x11\nvpclmulqdq {x0}, {x0}, {k}, 0x11\nvpternlogq {x3}, {t3}, {d3}, 0x96\nvpternlogq {x2}, {t2}, {d2}, 0x96\nvpternlogq {x1}, {t1}, {d1}, 0x96\nvpternlogq {x0}, {t0}, {d0}, 0x96",
                x3: inout(zmm_reg) x3, x2: inout(zmm_reg) x2, x1: inout(zmm_reg) x1, x0: inout(zmm_reg) x0,
                t3: out(zmm_reg) t3, t2: out(zmm_reg) t2, t1: out(zmm_reg) t1, t0: out(zmm_reg) t0,
                k: in(zmm_reg) kz, d3: in(zmm_reg) d3, d2: in(zmm_reg) d2, d1: in(zmm_reg) d1, d0: in(zmm_reg) d0,
                options(pure, nomem, nostack))
        }
        i += 256
    }
    // x3 onto x2, the sum onto x1, that onto x0: 64 bytes apart each.
    unsafe "vpclmulqdq and vpternlogq read and write only these zmm registers; the clause names avx512f and vpclmulqdq" {
        asm("vpclmulqdq {t3}, {x3}, {k}, 0x00\nvpclmulqdq {x3}, {x3}, {k}, 0x11\nvpternlogq {x2}, {t3}, {x3}, 0x96\nvpclmulqdq {t3}, {x2}, {k}, 0x00\nvpclmulqdq {x2}, {x2}, {k}, 0x11\nvpternlogq {x1}, {t3}, {x2}, 0x96\nvpclmulqdq {t3}, {x1}, {k}, 0x00\nvpclmulqdq {x1}, {x1}, {k}, 0x11\nvpternlogq {x0}, {t3}, {x1}, 0x96",
            x3: inout(zmm_reg) x3, x2: inout(zmm_reg) x2, x1: inout(zmm_reg) x1, x0: inout(zmm_reg) x0,
            t3: out(zmm_reg) t3, k: in(zmm_reg) k4, options(pure, nomem, nostack))
    }
    y := x0.ToArray()
    l3 := simd.Vec[u64, 2].FromArray([y[0], y[1]])
    l2 := simd.Vec[u64, 2].FromArray([y[2], y[3]])
    l1 := simd.Vec[u64, 2].FromArray([y[4], y[5]])
    l0 := simd.Vec[u64, 2].FromArray([y[6], y[7]])
    return CrcFour(l3, l2, l1, l0, data, i, fold)
}

/// Crc32Fold folds every whole 16-byte block of data, for CRC-32/ISO-HDLC
/// with the register c (pre-inverted, not the finished CRC), and reports
/// the bytes it consumed, data.Len() / 16 * 16: the CRC register from zero
/// over the 16 bytes it writes to fold equals c's over those bytes. It
/// reports 0, writing nothing, when data holds fewer than 64 bytes or the
/// CPU lacks pclmul and aes (always on x86_64-freestanding and at compile
/// time). O(n), two carry-less products per 16 bytes, four at a time
/// from 256 bytes on x86_64 with AVX-512 and VPCLMULQDQ; it allocates
/// nothing.
export Crc32Fold(c: u32, data: []u8, fold: mut [16]u8): int {
    if Target.os == Os.Freestanding || data.Len() < 64 {
        return 0
    }
    id := simd.Cpu()
    if !id.Has("pclmul") || !id.Has("aes") {
        return 0
    }
    var k := 0
    // The arch test leads so that a build for another architecture drops
    // the x86 asm of Crc32Wide at compile time.
    if Target.arch == Arch.X64 && data.Len() >= 256 && id.Has("avx512f") && id.Has("vpclmulqdq") {
        unsafe "Cpu() reports pclmul, aes, avx512f and vpclmulqdq, which Crc32Wide's clause names" {
            k = Crc32Wide(c, data, fold)
        }
        return k
    }
    unsafe "Cpu() reports pclmul and aes, which Crc32Ni's clause names" {
        k = Crc32Ni(c, data, fold)
    }
    return k
}
