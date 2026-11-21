// Why: str is the language's text type; its everyday methods (search, trim,
// split, compare prefixes) belong in the prelude and must be fast on long
// text without allocating.
//
// What: str.Len and str.AsBytes (intrinsics), Byte, Find, Has, StartsWith,
// EndsWith, Trim, Split, Lines, Words and Scalars, and the helpers FindFrom,
// TwoWay, MaxSuffix, BytesEqual, SpaceAt and LoadWord.
//
// How: a one-byte search is IndexByteIn's memchr; a longer one is
// core_find.mk's vector pair prefilter, handing over to the Two-Way
// algorithm when it stops paying, O(n + m) time and O(1) space. Byte
// compares and the one-byte search are std.simd's past their short paths,
// except on x86_64-freestanding, where std.core's own word-at-a-time forms
// cost fewer compile-time steps.

import std.raw
import std.simd

/// The length in bytes. O(1).
export str.Len(self): int
/// The text's bytes, sharing its memory. O(1); allocates nothing.
export str.AsBytes(self): []u8

/// The byte at offset i; panics (code 2) outside 0..Len(). O(1).
export str.Byte(self, i: int): u8 {
    return self.AsBytes()[i]
}

/// The byte offset of the first match of sub, or none; "" matches at 0. O(n +
/// m) in the worst case; allocates nothing.
export str.Find(self, sub: str): ?int {
    return FindFrom(self, sub, 0)
}

/// Whether sub occurs in the text; "" always does. O(n + m) in the worst
/// case; allocates nothing.
export str.Has(self, sub: str): bool {
    return FindFrom(self, sub, 0).IsSome()
}

/// Whether the text starts with prefix; "" always does, and returns before any
/// compare. O(prefix.Len()); allocates nothing. Above 16 bytes it compares
/// with vectors, except on x86_64-freestanding.
export str.StartsWith(self, prefix: str): bool {
    if prefix.Len() == 0 {
        return true
    }
    if prefix.Len() > self.Len() {
        return false
    }
    return BytesEqual(self.AsBytes(), 0, prefix.AsBytes())
}

/// Whether the text ends with suffix; "" always does. O(suffix.Len());
/// allocates nothing. Above 16 bytes it compares with vectors, except on
/// x86_64-freestanding.
export str.EndsWith(self, suffix: str): bool {
    if suffix.Len() == 0 {
        return true
    }
    if suffix.Len() > self.Len() {
        return false
    }
    return BytesEqual(self.AsBytes(), self.Len() - suffix.Len(), suffix.AsBytes())
}

/// The text without its leading and trailing White_Space scalars, as a
/// sub-view. O(n); allocates nothing.
export str.Trim(self): str {
    var start := 0
    var end := self.Len()
    // Both end bytes ASCII and not White_Space, the common case (a field, a
    // word): nothing to trim, so no call of SpaceAt or SpaceBefore. Every
    // ASCII White_Space byte is below 64, AsciiSpace's bits; an ASCII last
    // byte is a whole scalar, so SpaceBefore would look at it alone.
    if end == 0 {
        return self
    }
    b := self.AsBytes()
    f := b[0]
    l := b[end - 1]
    if f < 0x80 && l < 0x80 && (f >= 64 || ((AsciiSpace >> u64(f)) & 1) == 0) && (l >= 64 || ((AsciiSpace >> u64(l)) & 1) == 0) {
        return self
    }
    for start < end {
        n := SpaceAt(self, start)
        if n == 0 {
            break
        }
        start += n
    }
    for end > start {
        n := SpaceBefore(self, end)
        if n == 0 {
            break
        }
        end -= n
    }
    return self[start..end]
}

/// An iterator over the pieces between occurrences of sep, empty pieces kept.
/// An empty sep panics here (code 8). O(1); allocates nothing.
export str.Split(self, sep: str): Split from self, sep {
    if sep.Len() == 0 {
        raw.Fail(8, "str.Split: empty separator")
    }
    return Split(text: self, sep: sep, pos: 0, done: false)
}

/// An iterator over the lines, each without its \n or \r\n; a final newline
/// ends the last line without starting an empty one. O(1); allocates nothing.
export str.Lines(self): Lines {
    return Lines(text: self, pos: 0)
}

/// An iterator over the words, split on White_Space. O(1); allocates nothing.
export str.Words(self): Words {
    return Words(text: self, pos: 0)
}

/// An iterator over the Unicode scalars. O(1); allocates nothing.
export str.Scalars(self): Scalars {
    return Scalars(text: self, pos: 0)
}

/// The byte offset of the first match of sub in s at or after from, or
/// none; "" matches at from. A from outside 0..=s.Len() is a bounds panic
/// (code 2). O(n + m) time in the worst case and O(1) space; allocates
/// nothing.
///
/// A one-byte sub is a byte scan, vectorized past 32 bytes. A longer one is
/// a vector prefilter: two bytes of sub (rare ones on a long text) are
/// compared at 16 starts a step (32 where the CPU has avx2), and each start
/// where both are in place is verified. When verification stops paying
/// (many near misses, as with "aaa...ab" in "aaa...a"), the rest of the
/// search is the Two-Way algorithm (Crochemore and Perrin), which keeps the
/// call linear. On x86_64-freestanding it is Two-Way alone.
export FindFrom(s: str, sub: str, from: int): ?int {
    n := s.Len()
    m := sub.Len()
    if from < 0 || from > n {
        raw.Fail(2, "FindFrom: start out of range")
    }
    if m == 0 {
        return some(from)
    }
    if m == 1 {
        return IndexByteIn(s.AsBytes(), sub.Byte(0), from)
    }
    if m > n - from {
        return none
    }
    b := s.AsBytes()
    p := sub.AsBytes()
    r := PairFind(b, p, from)
    if r >= 0 {
        return some(r)
    }
    if r == -1 {
        return none
    }
    return TwoWay(b, p, -2 - r)
}

// TwoWay is the Two-Way search of p in b from start from, where 2 <=
// p.Len() (FindFrom's checks): the offset of the first match, or none. It
// can skip a needle length when the byte under the needle's last position
// does not occur in it.
TwoWay(b: []u8, p: []u8, from: int): ?int {
    n := b.Len()
    m := p.Len()
    if m > n - from {
        return none
    }
    l1 := MaxSuffix(p, false)
    l2 := MaxSuffix(p, true)
    var crit := l1.left
    var period := l1.period
    if l2.left > l1.left {
        crit = l2.left
        period = l2.period
    }
    // A short period (p[..crit] repeats at period) keeps a memory of the
    // prefix already matched; a long one needs none and shifts further.
    long := !BytesEqual(p, period, p[0..crit])
    if long {
        period = IntMax(crit, m - crit) + 1
    }
    var set: u64 = 0
    for c in p {
        set = set | (Trunc[u64](1) << (c & 63))
    }
    var pos := from
    var mem := 0
    for pos <= n - m {
        if (set >> (b[pos + m - 1] & 63)) & 1 == 0 {
            pos += m
            mem = 0
            continue
        }
        var i := crit
        if !long && mem > crit {
            i = mem
        }
        for i < m && p[i] == b[pos + i] {
            i += 1
        }
        if i < m {
            pos += i - crit + 1
            mem = 0
            continue
        }
        lo := if long { 0 } else { mem }
        var j := crit - 1
        for j >= lo && p[j] == b[pos + j] {
            j -= 1
        }
        if j >= lo {
            pos += period
            if !long {
                mem = m - period
            }
            continue
        }
        return some(pos)
    }
    return none
}

// A maximal suffix of a needle: where it starts and its period.
struct Suffix {
    left: int
    period: int
}

// MaxSuffix is the maximal suffix of p under byte order, or under the
// reversed order when greater (the critical factorization takes the later
// of the two). O(p.Len()).
MaxSuffix(p: []u8, greater: bool): Suffix {
    var left := 0
    var right := 1
    var offset := 0
    var period := 1
    for right + offset < p.Len() {
        a := p[right + offset]
        b := p[left + offset]
        if (!greater && a < b) || (greater && a > b) {
            right += offset + 1
            offset = 0
            period = right - left
        } else if a == b {
            if offset + 1 == period {
                right += offset + 1
                offset = 0
            } else {
                offset += 1
            }
        } else {
            left = right
            right += 1
            offset = 0
            period = 1
        }
    }
    return Suffix(left: left, period: period)
}

/// Whether b holds the bytes of p at offset at. The caller keeps at +
/// p.Len() <= b.Len(); a byte read outside b is a bounds panic (code 2).
/// O(p.Len()); allocates nothing.
///
/// Above 16 bytes it compares four vectors per step, wider where the CPU
/// has avx2 or avx512bw. From 8 to 16 bytes it compares two words, which
/// may overlap, and below 8 bytes it compares bytes. On x86_64-freestanding
/// it compares eight bytes at a time at every length, then the tail.
export BytesEqual(b: []u8, at: int, p: []u8): bool {
    m := p.Len()
    if Target.os != Os.Freestanding {
        if m > 16 {
            return simd.Equal(b[at..at + m], p)
        }
        if m >= 8 {
            return LoadWord(b, at) == LoadWord(p, 0) && LoadWord(b, at + m - 8) == LoadWord(p, m - 8)
        }
    }
    var j := 0
    if Target.os == Os.Freestanding {
        for j + 8 <= m {
            if LoadWord(b, at + j) != LoadWord(p, j) {
                return false
            }
            j += 8
        }
    }
    for j < m {
        if b[at + j] != p[j] {
            return false
        }
        j += 1
    }
    return true
}

/// The byte length of the Unicode White_Space scalar that starts at byte i
/// of s, or 0 when the scalar there is not White_Space. O(1); allocates
/// nothing.
export SpaceAt(s: str, i: int): int {
    b := s.AsBytes()
    c := b[i]
    if c < 0x80 {
        if c < 64 && ((AsciiSpace >> u64(c)) & 1) != 0 {
            return 1
        }
        return 0
    }
    k := ScalarLen(c)
    if k > s.Len() - i {
        return 0
    }
    var v := u32(c) & (0x7F >> Trunc[u32](k))
    for j in 1..k {
        v = (v << 6) | (u32(b[i + j]) & 0x3F)
    }
    for r in WhiteSpace {
        if v < r.lo {
            return 0
        }
        if v <= r.hi {
            return k
        }
    }
    return 0
}

/// The byte length of the Unicode White_Space scalar that ends at byte end
/// of s, or 0 when the scalar there is not White_Space. O(1); allocates
/// nothing.
export SpaceBefore(s: str, end: int): int {
    b := s.AsBytes()
    var start := end - 1
    for start > 0 && (b[start] & 0xC0) == 0x80 && end - start < 4 {
        start -= 1
    }
    if SpaceAt(s, start) == end - start {
        return end - start
    }
    return 0
}

/// The 8 bytes of s at offset i as a little-endian u64. Panics (code 2)
/// unless 0 <= i and i + 8 <= s.Len(). O(1).
export LoadU64(s: str, i: int): u64 {
    return LoadWord(s.AsBytes(), i)
}

// LoadWord is the 8 bytes of b at i as a little-endian word, read with one
// align 1 load (raw.ReadUnaligned): text and byte views start at any
// address. Needs 0 <= i and i + 8 <= b.Len(), else a
// bounds panic.
LoadWord(b: []u8, i: int): u64 {
    if i < 0 || b.Len() - i < 8 {
        raw.Fail(2, "LoadWord: index out of range")
    }
    unsafe "i + 8 <= b.Len(): the 8 bytes are inside b" {
        return raw.ReadUnaligned(raw.PtrCast[u8, u64](raw.Offset(raw.Data(b), i)))
    }
}
