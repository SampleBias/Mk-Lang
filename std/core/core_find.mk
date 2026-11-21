// Why: substring search (str.Find, str.Has, FindFrom, Split, Replace) runs
// on every line of a text program, mostly on short haystacks and mostly
// without a match; Two-Way alone pays its needle analysis on every call and
// steps one byte at a time.
//
// What: PairFind, the vector prefilter in front of FindFrom's Two-Way
// search, with its pair choice (FindPair, PairRank) and its kernels
// PairFindN and PairFindAvx2.
//
// How: Mula's SIMD-friendly substring search, the memchr crate's packed
// pair. Two bytes of the needle are compared at N starts a step (two loads
// at the two places, two compares, one AND); each start where both are in
// place is verified with BytesEqual. From 256 starts the two are chosen
// rare by a frequency rank (O(m) table reads); below, they are the first
// and last bytes. Fewer than N starts are tried one by one, the last N with
// one overlapping step. Verification is budgeted: once the bytes it
// compared pass the bytes scanned plus a slack of a few needle lengths, the
// kernel stops and hands its position to Two-Way, so a call stays O(n + m)
// whatever the text. 16 lanes is the baseline; 32 where Cpu() reports avx2,
// from 64 starts. On x86_64-freestanding there is no prefilter: Two-Way
// alone.

import std.raw
import std.simd

// PairRankMin is the start count from which PairFind ranks the needle's
// bytes for its pair; below it the pair is the first and last bytes.
const PairRankMin: int = 256

// PairAvx2Min is the start count from which PairFind tests Cpu() for avx2:
// below it the 16-lane kernel finishes in a few steps.
const PairAvx2Min: int = 64

// PairRank is the rank of every byte as FindPair sees it: higher is
// commoner in text. Space first, the lowercase letters in English frequency
// order, line breaks and the punctuation of prose, logs and code, digits,
// uppercase letters at half their lowercase rank, other printable ASCII,
// UTF-8 bytes, and control bytes last. Only the order matters.
const PairRank: [256]u8 = PairRanks()

// PairLetters is the rank of a to z: English letter frequency order
// "etaoinshrdlcumwfgypbvkjxqz" as 250 minus 6 a place.
const PairLetters: [26]u8 = [238, 136, 184, 196, 250, 160, 154, 208, 226, 118, 124, 190, 172, 220, 232, 142, 106, 202, 214, 244, 178, 130, 166, 112, 148, 100]

PairRanks(): [256]u8 {
    var t: [256]u8 = [10; 256]
    for c in 0..256 {
        x := Trunc[u8](c)
        var r: u8 = 10
        if x == 0x20 {
            r = 255
        } else if x >= 0x61 && x <= 0x7A {
            r = PairLetters[c - 0x61]
        } else if x == 0x0A || x == 0x2E || x == 0x2C || x == 0x3D || x == 0x3A || x == 0x2F || x == 0x5F || x == 0x2D || x == 0x22 || x == 0x27 || x == 0x28 || x == 0x29 || x == 0x3B {
            r = 150
        } else if x >= 0x30 && x <= 0x39 {
            r = 120
        } else if x >= 0x41 && x <= 0x5A {
            r = PairLetters[c - 0x41] / 2
        } else if (x >= 0x21 && x <= 0x7E) || x == 0x09 || x == 0x0D {
            r = 80
        } else if x >= 0x80 {
            r = 60
        }
        t[c] = r
    }
    return t
}

// The two places of a needle the prefilter compares, lo < hi.
struct Pair {
    lo: int
    hi: int
}

// FindPair is the pair for p (at least 2 bytes): the byte of lowest rank,
// and the lowest-ranked byte at least max(2, m / 2) places from it, the
// earlier on a tie; any other byte when none is that far. Bytes close
// together are not independent in text ("=5" of "status=5" is in place in
// every "dur=5..."), so the second is taken apart from the first, toward
// Mula's first and last. O(m).
FindPair(p: []u8): Pair {
    m := p.Len()
    if m == 2 {
        return Pair(lo: 0, hi: 1)
    }
    var a := 0
    var ra := PairRank[Cast[int](p[0])]
    for k in 1..m {
        r := PairRank[Cast[int](p[k])]
        if r < ra {
            a = k
            ra = r
        }
    }
    gap := if m / 2 < 2 { 2 } else { m / 2 }
    var b := -1
    var rb: u8 = 0
    for k in 0..m {
        if k + gap <= a || k >= a + gap {
            r := PairRank[Cast[int](p[k])]
            if b < 0 || r < rb {
                b = k
                rb = r
            }
        }
    }
    if b < 0 {
        for k in 0..m {
            if k != a {
                r := PairRank[Cast[int](p[k])]
                if b < 0 || r < rb {
                    b = k
                    rb = r
                }
            }
        }
    }
    if a < b {
        return Pair(lo: a, hi: b)
    }
    return Pair(lo: b, hi: a)
}

// PairFind searches b for p from start from, where 2 <= p.Len() and
// from + p.Len() <= b.Len(): the offset of the first match (0 or more), -1
// when there is none, or -2 - i when the verification budget ran out with
// every start before i tried, for Two-Way to go on from i.
PairFind(b: []u8, p: []u8, from: int): int {
    if Target.os != Os.Freestanding {
        l := b.Len() - p.Len() + 1
        // Ranking costs two passes over the needle, as much as scanning a
        // short haystack; there the first and last bytes (Mula's pair)
        // do, and a poor pair costs at most a few verifications.
        pr := if l - from >= PairRankMin { FindPair(p) } else { Pair(lo: 0, hi: p.Len() - 1) }
        if l - from >= PairAvx2Min && simd.Cpu().Has("avx2") {
            unsafe "Cpu() reports avx2, which PairFindAvx2's clause names" {
                return PairFindAvx2(b, p, pr, from)
            }
        }
        return PairFindN[16](b, p, pr, from)
    }
    return -2 - from
}

PairFindAvx2(b: []u8, p: []u8, pr: Pair, from: int): int cpu "avx2" {
    return PairFindN[32](b, p, pr, from)
}

// PairFindN is PairFind with the pair pr chosen, at N starts a step.
PairFindN[const N: int](b: []u8, p: []u8, pr: Pair, from: int): int
    requires N == 2 || N == 4 || N == 8 || N == 16 || N == 32 || N == 64 {
    m := p.Len()
    // The l starts 0..l: the two loads of a step run over views of
    // length l offset by lo and hi, so one test bounds both.
    l := b.Len() - m + 1
    lo := pr.lo
    hi := pr.hi
    x := p[lo]
    y := p[hi]
    var i := from
    if l - i < N {
        for i < l {
            if b[i + lo] == x && b[i + hi] == y && BytesEqual(b, i, p) {
                return i
            }
            i += 1
        }
        return -1
    }
    vx := simd.Vec[u8, N].Splat(x)
    vy := simd.Vec[u8, N].Splat(y)
    w1 := b[lo..lo + l]
    w2 := b[hi..hi + l]
    // Verification may compare up to slack bytes more than the scan has
    // passed before Two-Way takes over.
    slack := 8 * m + 256
    var work := 0
    for i + N <= l {
        var mask := ((simd.Vec[u8, N].FromSlice(w1, i) == vx) & (simd.Vec[u8, N].FromSlice(w2, i) == vy)).Bits()
        for mask != 0 {
            c := i + raw.Ctz64(mask)
            if BytesEqual(b, c, p) {
                return c
            }
            work += m
            // Tested at each failed start, so one step's candidates
            // cannot overrun the budget by N needle lengths; every start
            // to c has been tried.
            if work > c + 1 - from + slack {
                return -2 - (c + 1)
            }
            mask = mask & (mask - 1)
        }
        i += N
    }
    if i < l {
        // The last N starts, of which the first N - (l - i) were tried.
        t := l - N
        var mask := ((simd.Vec[u8, N].FromSlice(w1, t) == vx) & (simd.Vec[u8, N].FromSlice(w2, t) == vy)).Bits() >> (N - (l - i))
        for mask != 0 {
            c := i + raw.Ctz64(mask)
            if BytesEqual(b, c, p) {
                return c
            }
            mask = mask & (mask - 1)
        }
    }
    return -1
}
