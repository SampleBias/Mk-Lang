// Why: Cur.Next and Cur.Want read the short form themselves and hand
// every other form to std.encoding's reader (NextDer), CheckName walks a
// Name of short forms in one loop (NameShort) and any other with the
// cursor (CheckNameCur), and ReadExtensions reads a short-form extension
// in place, so each pair must give the same answers.
// What: Next and Want against NextDer on generated DER-like bytes at every
// start and end; CheckName against CheckNameCur on mutated and generated
// Names; ReadExtensionsBy in place against the cursor on a mutated list.
// How: identifiers and lengths drawn from the bytes where the forms part
// (0x1F, 0x80 to 0x83, lengths that just fit or do not), compared as the
// element's fields or the failure's offset, and the cursor's position; the
// Names from a small grammar with wrong tags, lengths and contents drawn
// in.

// CurStep is one step as the test compares it: 0 and the element's tag,
// class, constructed bit, start, content start and end, or 1 and the
// failure's offset. kind picks the step: 0 Next, 1 NextDer, 2 Want and 3
// Want by NextDer (CurRefWant), with tag and cons.
CurStep(c: mut Cur, kind: int, tag: u32, cons: bool): [7]int {
    if kind == 0 {
        el := c.Next() else e {
            return [1, e.offset, 0, 0, 0, 0, 0]
        }
        return CurFields(&el)
    }
    if kind == 1 {
        el := c.NextDer() else e {
            return [1, e.offset, 0, 0, 0, 0, 0]
        }
        return CurFields(&el)
    }
    if kind == 2 {
        el := c.Want(tag, cons) else e {
            return [1, e.offset, 0, 0, 0, 0, 0]
        }
        return CurFields(&el)
    }
    el := CurRefWant(c, tag, cons) else e {
        return [1, e.offset, 0, 0, 0, 0, 0]
    }
    return CurFields(&el)
}

// CurFields is a read element as CurStep gives it.
CurFields(el: &El): [7]int {
    return [0, int(el.tag), int(el.class), if el.cons { 1 } else { 0 }, el.start, el.at, el.end]
}

// CurSame is whether two steps are the same.
CurSame(x: [7]int, y: [7]int): bool {
    for i in 0..7 {
        if x[i] != y[i] {
            return false
        }
    }
    return true
}

// CurRefWant is Want by NextDer.
CurRefWant(c: mut Cur, tag: u32, cons: bool): Malformed!El {
    e := try c.NextDer()
    if e.class != 0 || e.tag != tag || e.cons != cons {
        fail Malformed(offset: e.start)
    }
    return e
}

// CurByte draws a byte: often one where the identifier and length forms
// part, else any byte.
CurByte(st: mut u64): u8 {
    *st = *st *% 6364136223846793005 +% 1442695040888963407
    x := *st >> 33
    picks: [16]u8 = [0x30, 0x31, 0x02, 0x06, 0x1F, 0x3F, 0xA0, 0xA3, 0x80, 0x81, 0x82, 0x83, 0x00, 0x01, 0x7F, 0x05]
    if x % 3 == 0 {
        return Trunc[u8](x >> 8)
    }
    return picks[Cast[int](x % 16)]
}

test "Cur.Next and Cur.Want give NextDer's answers" (sys: &System) {
    _ = sys
    var st: u64 = 1
    var buf: [24]u8 = [0; 24]
    wants: [4]u32 = [16, 2, 6, 3]
    for round in 0..4000 {
        n := 1 + round % 24
        for i in 0..n {
            buf[i] = CurByte(mut st)
        }
        if round % 2 == 0 && n > 2 {
            // A length that fits exactly, or one byte too long.
            buf[1] = Trunc[u8](n - 2 + round % 4 / 2)
        }
        der := buf[0..n]
        for pos in 0..n + 1 {
            for end in pos..n + 1 {
                var x := Cur(der: der, pos: pos, end: end)
                var y := Cur(der: der, pos: pos, end: end)
                Assert(CurSame(CurStep(mut x, 0, 0, false), CurStep(mut y, 1, 0, false)))
                Assert(x.pos == y.pos)
                for k in 0..4 {
                    for c in 0..2 {
                        var p := Cur(der: der, pos: pos, end: end)
                        var q := Cur(der: der, pos: pos, end: end)
                        Assert(CurSame(CurStep(mut p, 2, wants[k], c == 1), CurStep(mut q, 3, wants[k], c == 1)))
                        Assert(p.pos == q.pos)
                    }
                }
            }
        }
    }
}

// NameResult is CheckName's (cur false) or CheckNameCur's (cur true)
// answer on n: -1 for ok, else the failure's offset.
NameResult(der: []u8, n: &El, cur: bool): int {
    if cur {
        CheckNameCur(der, n) else e {
            return e.offset
        }
        return -1
    }
    CheckName(der, n) else e {
        return e.offset
    }
    return -1
}

test "CheckName gives CheckNameCur's answers" (sys: &System) {
    _ = sys
    // A Name of two RDNs (O=Mk test, CN=Mk Test Root), as in the bench chain.
    name: [43]u8 = [0x30, 0x29, 0x31, 0x10, 0x30, 0x0e, 0x06, 0x03, 0x55, 0x04, 0x0a, 0x13, 0x07, 0x4d, 0x6b, 0x20, 0x74, 0x65, 0x73, 0x74, 0x31, 0x15, 0x30, 0x13, 0x06, 0x03, 0x55, 0x04, 0x03, 0x13, 0x0c, 0x4d, 0x6b, 0x20, 0x54, 0x65, 0x73, 0x74, 0x20, 0x52, 0x6f, 0x6f, 0x74]
    var m: [43]u8 = [0; 43]
    var plain := 0
    values: [12]u8 = [0x00, 0x01, 0x06, 0x0c, 0x13, 0x16, 0x1e, 0x1f, 0x30, 0x31, 0x80, 0x81]
    for p in 0..43 {
        for k in 0..12 + 1 {
            for i in 0..43 {
                m[i] = name[i]
            }
            if k < 12 {
                m[p] = values[k]
            } else {
                m[p] = name[p] ^ 0x01
            }
            for end in 2..44 {
                der := m[0..end]
                var top := Cur(der: der, pos: 0, end: end)
                el := top.Next() else {
                    continue
                }
                Assert(NameResult(der, &el, false) == NameResult(der, &el, true))
                if NameShort(der, &el) {
                    plain += 1
                }
            }
        }
    }
    // The unmutated Name, and mutations inside its strings, take the
    // one-loop walk.
    Assert(plain > 0)
}

// NameGen builds a generated Name in buf: SEQUENCE OF SET OF SEQUENCE {
// OID, value }, with element counts, tags, contents and length forms
// drawn by CurByte's generator, so the one-loop walk meets each of its
// refusals and the cursor walk each of its faults.
struct NameGen {
    buf: [160]u8
    n: int
    st: u64
}

NameGen.Draw(mut self, k: int): int {
    self.st = self.st *% 6364136223846793005 +% 1442695040888963407
    return Cast[int]((self.st >> 33) % Cast[u64](k))
}

NameGen.Put(mut self, b: u8) {
    if self.n < 160 {
        self.buf[self.n] = b
        self.n += 1
    }
}

// Open writes an identifier and a one-byte length to patch; Close patches
// it with the content's length, one too many at times.
NameGen.Open(mut self, id: u8): int {
    self.Put(id)
    at := self.n
    self.Put(0)
    return at
}

NameGen.Close(mut self, at: int) {
    len := self.n - at - 1
    if at < 160 {
        off := if self.Draw(16) == 0 { 1 } else { 0 }
        self.buf[at] = Trunc[u8](len + off)
    }
}

NameGen.Name(mut self) {
    top := self.Open(0x30)
    for _ in 0..self.Draw(3) {
        set := self.Open(if self.Draw(16) == 0 { 0x30 } else { 0x31 })
        for _ in 0..self.Draw(3) {
            atv := self.Open(0x30)
            if self.Draw(16) != 0 {
                oid := self.Open(if self.Draw(16) == 0 { 0x05 } else { 0x06 })
                oids: [6]u8 = [0x55, 0x04, 0x03, 0x80, 0x01, 0x2a]
                start := self.Draw(6)
                for k in start..start + self.Draw(4) {
                    self.Put(oids[k % 6])
                }
                self.Close(oid)
            }
            if self.Draw(16) != 0 {
                tags: [10]u8 = [0x13, 0x0c, 0x16, 0x12, 0x14, 0x1e, 0x04, 0x33, 0x1f, 0x93]
                v := self.Open(tags[self.Draw(10)])
                chars: [8]u8 = [0x41, 0x2a, 0x80, 0x00, 0x20, 0x31, 0xff, 0x26]
                for _ in 0..self.Draw(5) {
                    self.Put(chars[self.Draw(8)])
                }
                self.Close(v)
            }
            if self.Draw(16) == 0 {
                self.Put(0x05)
                self.Put(0x00)
            }
            self.Close(atv)
        }
        self.Close(set)
    }
    self.Close(top)
}

test "CheckName gives CheckNameCur's answers on generated Names" (sys: &System) {
    _ = sys
    var g := NameGen(buf: [0; 160], n: 0, st: 7)
    var plain := 0
    for _ in 0..20000 {
        g.n = 0
        g.Name()
        der := g.buf[0..g.n]
        var top := Cur(der: der, pos: 0, end: g.n)
        el := top.Next() else {
            continue
        }
        Assert(NameResult(der, &el, false) == NameResult(der, &el, true))
        if NameShort(der, &el) {
            plain += 1
        }
    }
    Assert(plain > 100)
}

// ExtCode is ReadExtensionsBy's answer on list as one line of ints: the
// failure's offset, or -1 and the Ext's fields.
ExtCode(der: []u8, list: &El, short_form: bool): [10]int {
    x := ReadExtensionsBy(der, list, short_form) else e {
        return [e.offset, 0, 0, 0, 0, 0, 0, 0, 0, 0]
    }
    return [-1, int(x.flags), x.path_len, int(x.key_usage), x.san.at, x.san.end, x.permitted.at, x.permitted.end, x.excluded.at, x.excluded.end]
}

// ExtSame is whether two ExtCode answers are the same.
ExtSame(x: [10]int, y: [10]int): bool {
    for i in 0..10 {
        if x[i] != y[i] {
            return false
        }
    }
    return true
}

test "ReadExtensions in place gives the cursor's answers" (sys: &System) {
    _ = sys
    // The bench leaf's extensions (key usage, critical; extended key usage;
    // authority key identifier; subjectAltName), after a long-form
    // SEQUENCE header.
    exts: [132]u8 = [
        0x30, 0x0e, 0x06, 0x03, 0x55, 0x1d, 0x0f, 0x01, 0x01, 0xff, 0x04, 0x04, 0x03, 0x02, 0x07,
        0x80, 0x30, 0x13, 0x06, 0x03, 0x55, 0x1d, 0x25, 0x04, 0x0c, 0x30, 0x0a, 0x06, 0x08, 0x2b,
        0x06, 0x01, 0x05, 0x05, 0x07, 0x03, 0x01, 0x30, 0x1f, 0x06, 0x03, 0x55, 0x1d, 0x23, 0x04,
        0x18, 0x30, 0x16, 0x80, 0x14, 0x7c, 0x1d, 0xe2, 0xa7, 0x86, 0xfe, 0x74, 0x4a, 0x91, 0x03,
        0xc6, 0xee, 0xd7, 0x16, 0x56, 0x12, 0x7a, 0x85, 0xd1, 0x0a, 0x30, 0x3c, 0x06, 0x03, 0x55,
        0x1d, 0x11, 0x04, 0x35, 0x30, 0x33, 0x82, 0x0c, 0x65, 0x78, 0x61, 0x6d, 0x70, 0x6c, 0x65,
        0x2e, 0x74, 0x65, 0x73, 0x74, 0x82, 0x0b, 0x2a, 0x2e, 0x77, 0x69, 0x6c, 0x64, 0x2e, 0x74,
        0x65, 0x73, 0x74, 0x87, 0x04, 0x7f, 0x00, 0x00, 0x01, 0x87, 0x10, 0x00, 0x00, 0x00, 0x00,
        0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01
    ]
    var m: [135]u8 = [0; 135]
    values: [14]u8 = [0x00, 0x01, 0x02, 0x03, 0x04, 0x06, 0x0a, 0x21, 0x30, 0x7f, 0x80, 0x81, 0xfe, 0xff]
    var same := 0
    for p in 0..132 {
        for k in 0..15 {
            m[0] = 0x30
            m[1] = 0x81
            m[2] = 132
            for i in 0..132 {
                m[3 + i] = exts[i]
            }
            m[3 + p] = if k < 14 { values[k] } else { exts[p] ^ 0x01 }
            for end in 3..136 {
                der := m[0..end]
                list := El(tag: 16, class: 0, cons: true, start: 0, at: 3, end: end)
                x := ExtCode(der, &list, true)
                Assert(ExtSame(x, ExtCode(der, &list, false)))
                if x[0] == -1 {
                    same += 1
                }
            }
        }
    }
    Assert(same > 0)
}
