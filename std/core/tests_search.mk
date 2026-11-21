// Why: Slice.Search runs a loop whose trip count depends only on the length
// and picks each half by a branch-free choice (raw.Select), and it reads
// each probe unchecked; it must give the index of Go's sort.Search loop on
// every length and every run of duplicates, with the call count it
// documents, and probe only elements of the slice whatever less answers.
// What: test blocks over every length 0 to 300 with runs of equal keys of
// several widths, every key present and absent, against a linear scan and
// TsGoSearch (sort.Search's loop) as oracles; seeded large slices against
// TsGoSearch; unsorted slices, where only the range of the result is
// promised; the calls of less; and a guard band around the slice that every
// probe must miss under less answers that follow no order.
// How: the keys are even, so every odd key is absent; less counts its calls
// through a captured Cell. In the guard-band test element k of the slice
// holds k and the elements around it hold values outside 0..n, so less sees
// a probe outside the slice as a value outside 0..n.

// TsGoSearch is Go's sort.Search loop over the elements less than key.
TsGoSearch(s: []int, key: int): int {
    var i := 0
    var j := s.Len()
    for i < j {
        h := i + (j - i) / 2
        if s[h] < key {
            i = h + 1
        } else {
            j = h
        }
    }
    return i
}

// TsLinear is the first index whose element is not less than key.
TsLinear(s: []int, key: int): int {
    var i := 0
    for i < s.Len() && s[i] < key {
        i += 1
    }
    return i
}

TsLess(e: &int, k: &int): bool {
    return *e < *k
}

// TsBitLen is the number of bits of n >= 0, ceil(log2(n + 1)).
TsBitLen(n: int): int {
    var b := 0
    var m := n
    for m > 0 {
        b += 1
        m = m >> 1
    }
    return b
}

TsNext(s: mut u64): u64 {
    *s = *s *% 6364136223846793005 +% 1442695040888963407
    return *s >> 33
}

test "Search gives sort.Search's index on every length and run of duplicates" (sys: &System) {
    var xs := List[int].New(sys.arena)
    for _ in 0..301 {
        try xs.Push(0)
    }
    widths: [6]int = [1, 2, 3, 7, 64, 1_000]
    for n in 0..301 {
        for w in widths {
            m := mut xs.ItemsMut()[0..n]
            for i in 0..n {
                m[i] = 2 * (i / w)
            }
            s := xs.Items()[0..n]
            top := if n == 0 { 0 } else { s[n - 1] }
            for key in -2..(top + 3) {
                calls := Cell[int].New(0)
                got := s.Search(&key, fn(e: &int, k: &int): bool {
                    calls.Store(calls.Load() + 1)
                    return *e < *k
                })
                Assert(got == TsLinear(s, key))
                Assert(got == TsGoSearch(s, key))
                Assert(calls.Load() == TsBitLen(n))
            }
        }
    }
}

test "Search matches sort.Search over large seeded slices" (sys: &System) {
    var seed: u64 = 11
    sizes: [5]int = [1_023, 1_024, 1_025, 65_537, 300_001]
    for n in sizes {
        var l := List[int].New(sys.arena)
        for _ in 0..n {
            try l.Push(Cast[int](TsNext(mut seed) % Cast[u64](n)) * 2)
        }
        l.Sort(TsLess)
        s := l.Items()
        for _ in 0..20_000 {
            key := Cast[int](TsNext(mut seed) % Cast[u64](2 * n + 4)) - 2
            Assert(s.Search(&key, TsLess) == TsGoSearch(s, key))
        }
        Assert(s.Search(&-1, TsLess) == 0 && s.Search(&(2 * n + 1), TsLess) == n)
    }
}

test "Search on an unsorted slice returns an index from 0 to Len" (sys: &System) {
    var seed: u64 = 3
    var l := List[int].New(sys.arena)
    for n in 0..80 {
        l.Clear()
        for _ in 0..n {
            try l.Push(Cast[int](TsNext(mut seed) % 16))
        }
        s := l.Items()
        for key in -1..18 {
            i := s.Search(&key, TsLess)
            Assert(i >= 0 && i <= n)
        }
        // A less that is no order at all.
        flip := Cell[bool].New(false)
        i := s.Search(&0, fn(e: &int, k: &int): bool {
            flip.Store(!flip.Load())
            return flip.Load()
        })
        Assert(i >= 0 && i <= n)
    }
}

// TsAnswer is less's answer on call c under mode m: 0 always, 1 never,
// 2 alternating, 3 bit c of bits, 4 the element below t.
TsAnswer(m: int, c: int, bits: u64, v: int, t: int): bool {
    if m == 0 {
        return true
    } else if m == 1 {
        return false
    } else if m == 2 {
        return c % 2 == 0
    } else if m == 3 {
        return (bits >> Cast[u64](c % 64)) & 1 == 1
    }
    return v < t
}

test "Search probes only elements of the slice whatever less answers" (sys: &System) {
    guard := 8
    var l := List[int].New(sys.arena)
    var seed: u64 = 17
    for n in 0..301 {
        l.Clear()
        for k in 0..guard {
            try l.Push(-1 - k)
        }
        for k in 0..n {
            try l.Push(k)
        }
        for k in 0..guard {
            try l.Push(n + k)
        }
        s := l.Items()[guard..guard + n]
        for m in 0..5 {
            bits := TsNext(mut seed) << 31 ^ TsNext(mut seed)
            t := Cast[int](TsNext(mut seed) % Cast[u64](n + 1))
            calls := Cell[int].New(0)
            i := s.Search(&0, fn(e: &int, k: &int): bool {
                v := *e
                Assert(v >= 0 && v < n)
                c := calls.Load()
                calls.Store(c + 1)
                return TsAnswer(m, c, bits, v, t)
            })
            Assert(i >= 0 && i <= n && calls.Load() == TsBitLen(n))
            if m == 0 {
                Assert(i == n)
            } else if m == 1 {
                Assert(i == 0)
            } else if m == 4 {
                Assert(i == t)
            }
        }
    }
}
