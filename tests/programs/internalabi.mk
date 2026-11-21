// Values of 9 to 16 bytes made of integers and pointers across Mk calls, as a program sees them.
// On x86_64-windows a function only Mk calls directly takes and returns them in two registers
// (abi.LowerInternal), while a function used as a value, or one a `become` reaches, keeps the C
// ABI, which passes them by reference: every call here mixes the two.
// - Swap and Mid take and return a struct of two ints and a str; Wide takes nine such values, so
//   its later halves go on the stack, one argument split between the last register and the stack.
// - Find returns ?int, Parse an error union of int, both 16 bytes.
// - Twice calls Swap directly and Pick through a fn value from a const table, which keeps Pick on
//   the C ABI. Flip is named by Flips, a const read here, and also called directly: it keeps the
//   C ABI, and so do its direct calls.
// - Walk ends in a `become` of itself that passes its str and struct on unchanged.
// Expected output: internalabi.out.

error Bad(at: int)

struct Pair {
    a: int
    b: int
}

Swap(p: Pair): Pair {
    return Pair(a: p.b, b: p.a)
}

Mid(s: str, from: int): str {
    return s[from..s.Len() - from]
}

Wide(s1: str, p1: Pair, s2: str, p2: Pair, xs: []int, s3: str, p3: Pair, s4: str, p4: Pair): int {
    var t := s1.Len() * 1 + s2.Len() * 10 + s3.Len() * 100 + s4.Len() * 1000
    t += p1.a + p1.b * 2 + p2.a * 3 + p2.b * 4 + p3.a * 5 + p3.b * 6 + p4.a * 7 + p4.b * 8
    for x in xs {
        t += x
    }
    return t
}

Find(xs: []int, x: int): ?int {
    for i in 0..xs.Len() {
        if xs[i] == x {
            return some(i)
        }
    }
    return none
}

Parse(s: str): Bad!int {
    var n := 0
    for i in 0..s.Len() {
        c := s.Byte(i)
        if c < 48 || c > 57 {
            fail Bad(at: i)
        }
        n = n * 10 + Cast[int](c - 48)
    }
    return n
}

Pick(p: Pair): Pair {
    return Pair(a: p.a + p.b, b: p.a - p.b)
}

const Table: [2]fn(Pair): Pair = [Pick, Pick]

Flip(p: Pair): Pair {
    return Pair(a: -p.a, b: -p.b)
}

const Flips: [1]fn(Pair): Pair = [Flip]

Twice(p: Pair, k: int): Pair {
    q := Swap(p)
    return Table[k % 2](q)
}

Walk(n: int, s: str, p: Pair, acc: int): int {
    if n == 0 {
        return s.Len() + p.a * 100 + p.b + acc
    }
    become Walk(n - 1, s, p, acc + n)
}

export Main(sys: &System): WriteError!void {
    k := sys.args.Len() // 1: nothing folds to a constant
    p := Swap(Pair(a: 3 * k, b: 4 * k))
    try sys.Print("swap {a} {b}\n", (a: p.a, b: p.b))
    try sys.Print("mid [{s}]\n", (s: Mid("abcdefgh", 2 * k)))
    xs := [k, 2 * k, 3 * k]
    w := Wide("a", Pair(a: k, b: 2), "bb", Pair(a: 3, b: 4), xs[..], "ccc", Pair(a: 5, b: 6), "dddd", Pair(a: 7, b: 8 * k))
    try sys.Print("wide {w}\n", (w: w))
    if i := Find(xs[..], 2 * k) {
        try sys.Print("find {i}\n", (i: i))
    }
    if Find(xs[..], 9).IsNone() {
        try sys.Print("find none\n", ())
    }
    n := Parse("1234") else {
        -1
    }
    try sys.Print("parse {n}\n", (n: n))
    bad := Parse("12x4") else {
        -2
    }
    try sys.Print("parse bad {b}\n", (b: bad))
    f := Flips[k - 1](Flip(Pair(a: 1, b: 2 * k)))
    try sys.Print("flip {a} {b}\n", (a: f.a, b: f.b))
    t := Twice(Pair(a: 10 * k, b: 3), k)
    try sys.Print("twice {a} {b}\n", (a: t.a, b: t.b))
    try sys.Print("walk {w}\n", (w: Walk(5 * k, "hello", Pair(a: 1, b: 2), 0)))
}
