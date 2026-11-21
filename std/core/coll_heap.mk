// Why: an arena's containers die with the arena; data that outlives every
// arena needs containers whose memory comes from an explicit allocator, and
// a free with the wrong allocator must be caught, not corrupt the heap.
//
// What: HeapList, HeapMap, TextMap (with TextMapIter) and HeapText, each with
// New, its growth and lookup methods, and the release trio Free, FreeEmpty
// and FreeEach.
//
// How: each container records the allocator it was made with and compares it
// with the one passed in only when it allocates or frees (panic code 12 on a
// mismatch), so ordinary calls pay nothing. HeapMap and TextMap wrap the Swiss
// Table of coll_table.mk; TextMap keeps a key of up to 15 bytes inside its
// slot (TextKey) and copies a longer absent key into memory from the
// allocator. All are linear, so the program must release them.

import std.raw

/// A growable array whose buffer comes from an allocator, so it outlives any
/// arena. It remembers the allocator it was made with, and a grow or free with
/// another one panics (code 12). Linear: end it with Free, FreeEmpty or
/// FreeEach.
export linear view struct HeapList[T] {
    ptr: *T
    n: int
    cap: int
    owner: *Alloc
    marker: [0]&T
}

/// A hash map whose table comes from an allocator, a Swiss table. It hashes
/// and orders its entries as Map does. It remembers its allocator, and an
/// allocation or free with another one panics (code 12). Linear: end it with
/// Free, FreeEmpty or FreeEach.
export linear view struct HeapMap[K: Hash + Eq, V] {
    t: Table[K, V]
    owner: *Alloc
    mk: [0]&K
    mv: [0]&V
}

/// A hash map from text to V whose table and key copies come from an
/// allocator. A new key of up to 15 bytes is kept inside the table, with no
/// allocation of its own; Set copies a longer new key into memory from a,
/// and Remove and Free give that memory back. It remembers its allocator,
/// and an allocation or free with another one panics (code 12). Linear: end
/// it with Free, FreeEmpty or FreeEach.
export linear view struct TextMap[V] {
    t: Table[TextKey, V]
    owner: *Alloc
    marker: [0]&V
}

/// An iterator over a TextMap's entries, in the map's iteration order. The
/// map stays borrowed while the iterator lives.
export view struct TextMapIter[V] {
    map: &Table[TextKey, V]
    i: int
}

/// A growable text buffer whose memory comes from an allocator. It remembers
/// its allocator, and a grow or free with another one panics (code 12).
/// Linear: end it with Free. It implements no Eq or Hash, so it is no
/// HeapMap key: a text key belongs in a TextMap.
export linear view struct HeapText {
    ptr: *u8
    used: int
    cap: int
    owner: *Alloc
}

unsafe impl[T: Send] Send for HeapList[T] {}
unsafe impl[T: Share] Share for HeapList[T] {}
// Send and Share if the contents are; the Table's raw
// pointers alone would make them neither.
unsafe impl[K: Hash + Eq + Send, V: Send] Send for HeapMap[K, V] {}
unsafe impl[K: Hash + Eq + Share, V: Share] Share for HeapMap[K, V] {}
unsafe impl[V: Send] Send for TextMap[V] {}
unsafe impl[V: Share] Share for TextMap[V] {}
unsafe impl Send for HeapText {}
unsafe impl Share for HeapText {}

impl[T] Slice[T] for HeapList[T] {
    // ptr holds n initialized elements, alive while self is.
    Items(&self): []T {
        return SlotView(self.ptr, self.n)
    }

    ItemsMut(mut self): mut []T {
        return SlotViewMut(self.ptr, self.n)
    }
}

// Prints [a, b, c] ([] when empty) through FmtElems (core_impls.mk).
impl[T: Format] Format for HeapList[T] {
    Format(&self, w: mut Writer): WriteError!void {
        try FmtElems(w, self.Items())
    }
}

/// An empty list that will allocate from a. O(1); allocates nothing until the
/// first Push.
export HeapList[T].New(a: &Alloc): HeapList[T] {
    return HeapList[T](ptr: Dangling[T](), n: 0, cap: 0, owner: raw.Of(a), marker: [])
}

// Grow makes room for need elements from a, after the owner check, which
// names op; the old buffer goes back to a.
HeapList[T].Grow(mut self, a: &Alloc, need: int, op: str): OutOfMemory!void {
    OwnerCheck(a, self.owner, op)
    c := NextCap(self.cap, need)
    p := GrowSlots(a, self.ptr, self.n, self.cap, c, true)
    if raw.IsNull(p) {
        fail OutOfMemory
    }
    self.ptr = p
    self.cap = c
}

/// Appends x. OutOfMemory when the buffer cannot grow, and then the caller
/// still owns x. Amortized O(1); a growth checks that a is the list's
/// allocator, allocates the new buffer from a and frees the old one. A Push
/// that does not grow does not look at a.
export HeapList[T].Push(mut self, a: &Alloc, x: T into self): OutOfMemory!void {
    if self.n == self.cap {
        try self.Grow(a, self.n + 1, "HeapList.Push")
    }
    SlotWrite(self.ptr, self.n, x)
    self.n += 1
}

/// Appends a copy of each element of xs, in order: one growth to fit them
/// all, then one Slice.CopyFrom of xs into the new slots, one memmove. The
/// elements are copied, not moved, so T must be plain: a call with any other
/// T is E0701. The list takes the loans the elements hold, not a loan on xs,
/// so xs may be a view of a buffer the caller keeps using.
/// OutOfMemory when the buffer cannot grow, and then the list is unchanged.
/// O(xs.Len()), plus a copy of Len() elements when it grows; a growth checks
/// that a is the list's allocator, allocates from a and frees the old buffer.
/// An Extend that does not grow, or of an empty xs, does not look at a.
export HeapList[T].Extend(mut self, a: &Alloc, xs: []T): OutOfMemory!void *xs into self {
    k := xs.Len()
    if k == 0 {
        return
    }
    if k > self.cap - self.n {
        try self.Grow(a, self.n + k, "HeapList.Extend")
    }
    // ptr has room for n + k elements; the last k are written, not read.
    // One CopyFrom, not an element loop: LLVM does not make that loop a
    // memcpy (its two slices do not tell it the arrays apart), so it
    // copies one element per step, 10 instructions per byte. No reachable
    // trap moves. The loop's checks, per element: self.n + i (code 1),
    // d[self.n + i] and xs[i] (code 2). None could fail: i < k, d.Len() is
    // self.n + k and xs.Len() is k, and self.n + k does not overflow here
    // (without growth it is at most cap; with growth, Grow's argument
    // computed it under its check first). Their replacements, once each:
    // the range's self.n + k (code 1, the same sum), the range check
    // 0 <= self.n <= self.n + k <= d.Len() (code 2; self.n >= 0 is the
    // list's invariant) and CopyFrom's length compare (code 2; both are
    // k), which cannot fail either. The one check that can fail, the sum
    // in Grow's argument, is unchanged and still comes first. An xs that
    // overlaps the list's own spare slots (only unsafe code can make one)
    // is copied as memmove copies, not forward element by element.
    d := SlotViewMut(self.ptr, self.n + k)
    (mut d[self.n..self.n + k]).CopyFrom(xs)
    self.n += k
}

/// Removes and returns the last element, or none when the list is empty. O(1);
/// the buffer is kept.
export HeapList[T].Pop(mut self): ?T {
    if self.n == 0 {
        return none
    }
    self.n -= 1
    return some(SlotRead(self.ptr, self.n))
}

/// The number of elements. O(1).
export HeapList[T].Len(&self): int {
    return self.n
}

/// Removes and returns element i, keeping the order of the rest. Panics
/// (code 2) unless 0 <= i < Len(). O(Len() - i); allocates nothing.
export HeapList[T].Remove(mut self, i: int): T {
    if i < 0 || i >= self.n {
        raw.Fail(2, "HeapList.Remove: index out of range")
    }
    x := SlotRead(self.ptr, i)
    SlotShift(self.ptr, i, i + 1, self.n - i - 1)
    self.n -= 1
    return x
}

/// Inserts x before element i (i == Len() appends). Panics (code 2) unless
/// 0 <= i <= Len(), before any growth. OutOfMemory when the buffer cannot
/// grow, and then the caller still owns x. O(Len() - i); growth allocates
/// from a.
export HeapList[T].Insert(mut self, a: &Alloc, i: int, x: T into self): OutOfMemory!void {
    if i < 0 || i > self.n {
        raw.Fail(2, "HeapList.Insert: index out of range")
    }
    if self.n == self.cap {
        try self.Grow(a, self.n + 1, "HeapList.Insert")
    }
    SlotShift(self.ptr, i + 1, i, self.n - i)
    SlotWrite(self.ptr, i, x)
    self.n += 1
}

/// Empties the list and keeps its buffer. A list that still holds linear
/// elements panics (code 11) rather than drop them. O(Len()); allocates
/// nothing.
export HeapList[T].Clear(mut self) {
    if self.n > 0 && raw.IsLinear[T]() {
        raw.Fail(11, "HeapList.Clear of linear elements")
    }
    for i in 0..self.n {
        Discard(SlotRead(self.ptr, i))
    }
    self.n = 0
}

/// Releases every element with a, in index order, then frees the buffer. An
/// a that is not the list's allocator panics (code 12). A call is E0701 when
/// T is linear and not Free, so the elements never cause a panic. O(1) for a
/// T with nothing to release, else O(n) plus the elements' own Free.
impl[T] Free for HeapList[T] {
    Free(self, a: &Alloc) {
        HeapList(ptr, n, cap, owner, marker) := self
        OwnerCheck(a, owner, "HeapList.Free")
        Discard(marker)
        if NeedsRelease[T]() {
            for i in 0..n {
                ReleaseValue(SlotRead(ptr, i), a)
            }
        }
        FreeSlots(a, ptr, cap)
    }
}

/// Frees the buffer of an empty list, with the list's own allocator a. Panics
/// (code 11) when the list still holds an element. It ends a list whose
/// element type is linear and not Free, after every element is taken out.
/// O(1).
export HeapList[T].FreeEmpty(self, a: &Alloc) {
    if self.n > 0 {
        raw.Fail(11, "HeapList.FreeEmpty of a non-empty list")
    }
    HeapList(ptr, n: _, cap, owner, marker) := self
    OwnerCheck(a, owner, "HeapList.FreeEmpty")
    Discard(marker)
    FreeSlots(a, ptr, cap)
}

/// Hands every element to release, in index order, then frees the buffer
/// with the list's own allocator a. release may capture and must consume its
/// argument. O(Len()) calls of release.
export HeapList[T].FreeEach(self, a: &Alloc, const release: fn(T)) {
    HeapList(ptr, n, cap, owner, marker) := self
    OwnerCheck(a, owner, "HeapList.FreeEach")
    Discard(marker)
    for i in 0..n {
        release(SlotRead(ptr, i))
    }
    FreeSlots(a, ptr, cap)
}

/// An empty map that will allocate from a. It hashes with a fixed function,
/// so its order is the same from run to run. The function may change between
/// releases, and it is not safe for keys from untrusted input: use NewKeyed.
/// O(1); allocates nothing until the first insert.
export HeapMap[K, V].New(a: &Alloc): HeapMap[K, V] {
    return HeapMap[K, V](t: EmptyTable[K, V](), owner: raw.Of(a), mk: [], mv: [])
}

/// As New, but hashing with SipHash-1-3 under key, for keys from
/// untrusted input. O(1); allocates nothing.
export HeapMap[K, V].NewKeyed(a: &Alloc, key: HashKey): HeapMap[K, V] {
    return HeapMap[K, V](t: EmptyTableKeyed[K, V](key), owner: raw.Of(a), mk: [], mv: [])
}

/// The value stored for k, or none. Expected O(1); allocates nothing.
export HeapMap[K, V].Get(&self, k: &K): ?&V {
    i := TableFind(&self.t, TableHash(&self.t, k), k)
    if i < 0 {
        return none
    }
    return some(SlotRef(ValueAt(self.t.slots, i), 0))
}

/// The value stored for k, writable in place, or none. Expected O(1); allocates
/// nothing.
export HeapMap[K, V].GetMut(mut self, k: &K): ?mut V {
    i := TableFind(&self.t, TableHash(&self.t, k), k)
    if i < 0 {
        return none
    }
    return some(SlotMut(ValueAt(self.t.slots, i), 0))
}

// Place is TablePlace with the owner check, made exactly when the insert
// allocates (one compare on grow and free only). Reusing a deleted
// slot and rehashing in place at growth 0 touch no memory of a, so they
// compare nothing; TablePlaceKind is the decision TablePlace
// itself takes.
HeapMap[K, V].Place(mut self, a: &Alloc, hash: u64, slot: int, op: str): int {
    if TablePlaceKind(&self.t, slot) == PlaceGrow {
        OwnerCheck(a, self.owner, op)
    }
    return TablePlace(mut self.t, a, true, hash, slot)
}

/// Sets k's value to v. When k is present the stored key stays, and the old
/// value and the given k are released with a; a call is E0701 when K or V is
/// linear and not Free. OutOfMemory when the table cannot grow for a new key,
/// and then the caller still owns k and v. Expected O(1); growth is amortized
/// O(1) and allocates from a.
export HeapMap[K, V].Set(mut self, a: &Alloc, k: K into self, v: V into self): OutOfMemory!void {
    hash := TableHash(&self.t, &k)
    r := TableLookup(&self.t, hash, &k)
    if r.at >= 0 {
        old := Replace(SlotMut(ValueAt(self.t.slots, r.at), 0), v)
        ReleaseValue(old, a)
        ReleaseValue(k, a)
        return
    }
    j := self.Place(a, hash, r.free, "HeapMap.Set")
    if j < 0 {
        fail OutOfMemory
    }
    SlotWrite(KeyAt(self.t.slots, j), 0, k)
    SlotWrite(ValueAt(self.t.slots, j), 0, v)
}

/// Sets k's value to v and returns the old value, or none when k was absent.
/// When k is present the given k is released with a; a call is E0701 when K
/// is linear and not Free. OutOfMemory as Set. Expected O(1); growth
/// allocates from a.
export HeapMap[K, V].Replace(mut self, a: &Alloc, k: K into self, v: V into self): OutOfMemory!?V {
    hash := TableHash(&self.t, &k)
    r := TableLookup(&self.t, hash, &k)
    if r.at >= 0 {
        old := Replace(SlotMut(ValueAt(self.t.slots, r.at), 0), v)
        ReleaseValue(k, a)
        return some(old)
    }
    j := self.Place(a, hash, r.free, "HeapMap.Replace")
    if j < 0 {
        fail OutOfMemory
    }
    SlotWrite(KeyAt(self.t.slots, j), 0, k)
    SlotWrite(ValueAt(self.t.slots, j), 0, v)
    return none
}

/// The value for k, writable, inserting v first when k is absent. When k is
/// present the given k and v are released with a; a call is E0701 when K or
/// V is linear and not Free. OutOfMemory as Set. Expected O(1); growth
/// allocates from a.
export HeapMap[K, V].Ensure(mut self, a: &Alloc, k: K into self, v: V into self): OutOfMemory!mut V from self {
    hash := TableHash(&self.t, &k)
    r := TableLookup(&self.t, hash, &k)
    if r.at >= 0 {
        ReleaseValue(k, a)
        ReleaseValue(v, a)
        return SlotMut(ValueAt(self.t.slots, r.at), 0)
    }
    j := self.Place(a, hash, r.free, "HeapMap.Ensure")
    if j < 0 {
        fail OutOfMemory
    }
    SlotWrite(KeyAt(self.t.slots, j), 0, k)
    SlotWrite(ValueAt(self.t.slots, j), 0, v)
    return SlotMut(ValueAt(self.t.slots, j), 0)
}

/// Removes k and returns its value, or none when k is absent. The stored key
/// is dropped, so a call with a linear K is E0701. Expected O(1); frees
/// nothing.
export HeapMap[K, V].Remove(mut self, k: &K): ?V {
    i := TableFind(&self.t, TableHash(&self.t, k), k)
    if i < 0 {
        return none
    }
    if raw.IsLinear[K]() {
        raw.Fail(8, "HeapMap.Remove would drop a linear value")
    }
    v := SlotRead(ValueAt(self.t.slots, i), 0)
    Discard(SlotRead(KeyAt(self.t.slots, i), 0))
    TableErase(mut self.t, i)
    return some(v)
}

/// The number of entries. O(1).
export HeapMap[K, V].Len(&self): int {
    return self.t.Count()
}

/// An iterator over the entries, in the map's unspecified order. O(1); a full
/// walk is O(capacity).
export HeapMap[K, V].Iter(&self): MapIter[K, V] {
    return MapIter[K, V](map: &self.t, i: 0)
}

/// Releases every key and value with a, then frees the table. An a that is
/// not the map's allocator panics (code 12). A call is E0701 when K or V is
/// linear and not Free, so the contents never cause a panic. O(1) when
/// neither K nor V has anything to release, since the table is not walked;
/// else O(capacity / 8) group reads plus the contents' own Free.
impl[K: Hash + Eq, V] Free for HeapMap[K, V] {
    Free(self, a: &Alloc) {
        HeapMap(t, owner, mk, mv) := self
        OwnerCheck(a, owner, "HeapMap.Free")
        Discard(mk)
        Discard(mv)
        if NeedsRelease[K]() || NeedsRelease[V]() {
            var left := t.used
            var i := 0
            for left > 0 {
                i = TableNext(&t, i)
                ReleaseValue(SlotRead(KeyAt(t.slots, i), 0), a)
                ReleaseValue(SlotRead(ValueAt(t.slots, i), 0), a)
                i += 1
                left -= 1
            }
        }
        TableFreeArrays(&t, a)
    }
}

/// Frees the table of an empty map, with the map's own allocator a. Panics
/// (code 11) when the map still holds an entry. It ends a map whose contents
/// are linear and not Free, after every entry is removed. O(1).
export HeapMap[K, V].FreeEmpty(self, a: &Alloc) {
    if self.t.used > 0 {
        raw.Fail(11, "HeapMap.FreeEmpty of a non-empty map")
    }
    HeapMap(t, owner, mk, mv) := self
    OwnerCheck(a, owner, "HeapMap.FreeEmpty")
    Discard(mk)
    Discard(mv)
    TableFreeArrays(&t, a)
}

/// Hands every key and value to release, in the map's iteration order, then
/// frees the table with the map's own allocator a. O(capacity / 8) group
/// reads plus Len() calls of release.
export HeapMap[K, V].FreeEach(self, a: &Alloc, const release: fn(K, V)) {
    HeapMap(t, owner, mk, mv) := self
    OwnerCheck(a, owner, "HeapMap.FreeEach")
    Discard(mk)
    Discard(mv)
    var left := t.used
    var i := 0
    for left > 0 {
        i = TableNext(&t, i)
        release(SlotRead(KeyAt(t.slots, i), 0), SlotRead(ValueAt(t.slots, i), 0))
        i += 1
        left -= 1
    }
    TableFreeArrays(&t, a)
}

/// An empty map that will allocate its table and key copies from a. It
/// hashes with a fixed function, so its order is the same from run to run.
/// The function may change between releases, and it is not safe for keys
/// from untrusted input: use NewKeyed. O(1); allocates nothing until the
/// first insert.
export TextMap[V].New(a: &Alloc): TextMap[V] {
    return TextMap[V](t: EmptyTable[TextKey, V](), owner: raw.Of(a), marker: [])
}

/// As New, but hashing with SipHash-1-3 under key, for keys from
/// untrusted input. O(1); allocates nothing.
export TextMap[V].NewKeyed(a: &Alloc, key: HashKey): TextMap[V] {
    return TextMap[V](t: EmptyTableKeyed[TextKey, V](key), owner: raw.Of(a), marker: [])
}

/// The value stored for k, or none. Expected O(1 + k.Len()); allocates nothing.
export TextMap[V].Get(&self, k: str): ?&V {
    p := TextProbe(&self.t, k)
    i := TableFind(&self.t, p.hash, &p.key)
    if i < 0 {
        return none
    }
    return some(SlotRef(ValueAt(self.t.slots, i), 0))
}

/// The value stored for k, writable in place, or none. Expected O(1 + k.Len());
/// allocates nothing.
export TextMap[V].GetMut(mut self, k: str): ?mut V {
    p := TextProbe(&self.t, k)
    i := TableFind(&self.t, p.hash, &p.key)
    if i < 0 {
        return none
    }
    return some(SlotMut(ValueAt(self.t.slots, i), 0))
}

/// Sets k's value to v. When k is present its old value is released with a;
/// a call is E0701 when V is linear and not Free. OutOfMemory when copying a
/// new key or growing the table fails, and then the caller still owns v.
/// Expected O(1 + k.Len()); a new key of 16 bytes or more is copied into
/// k.Len() bytes from a, which the map owns, and a shorter one is kept in
/// the table.
export TextMap[V].Set(mut self, a: &Alloc, k: str, v: V into self): OutOfMemory!void {
    p := TextProbe(&self.t, k)
    r := TableLookup(&self.t, p.hash, &p.key)
    if r.at >= 0 {
        ReleaseValue(Replace(SlotMut(ValueAt(self.t.slots, r.at), 0), v), a)
        return
    }
    j := self.KeySlot(a, p.hash, r.free, k, p.key, "TextMap.Set")
    if j < 0 {
        fail OutOfMemory
    }
    SlotWrite(ValueAt(self.t.slots, j), 0, v)
}

/// Sets k's value to v and returns the old value, or none when k was absent.
/// OutOfMemory as Set. Expected O(1 + k.Len()); a new key is copied, or kept
/// in the table, as Set does.
export TextMap[V].Replace(mut self, a: &Alloc, k: str, v: V into self): OutOfMemory!?V {
    p := TextProbe(&self.t, k)
    r := TableLookup(&self.t, p.hash, &p.key)
    if r.at >= 0 {
        return some(Replace(SlotMut(ValueAt(self.t.slots, r.at), 0), v))
    }
    j := self.KeySlot(a, p.hash, r.free, k, p.key, "TextMap.Replace")
    if j < 0 {
        fail OutOfMemory
    }
    SlotWrite(ValueAt(self.t.slots, j), 0, v)
    return none
}

/// The value for k, writable, inserting v first when k is absent. When k is
/// present the given v is released with a; a call is E0701 when V is linear
/// and not Free. OutOfMemory as Set. Expected O(1 + k.Len()); a new key is
/// copied, or kept in the table, as Set does.
export TextMap[V].Ensure(mut self, a: &Alloc, k: str, v: V into self): OutOfMemory!mut V from self {
    p := TextProbe(&self.t, k)
    r := TableLookup(&self.t, p.hash, &p.key)
    if r.at >= 0 {
        ReleaseValue(v, a)
        return SlotMut(ValueAt(self.t.slots, r.at), 0)
    }
    j := self.KeySlot(a, p.hash, r.free, k, p.key, "TextMap.Ensure")
    if j < 0 {
        fail OutOfMemory
    }
    SlotWrite(ValueAt(self.t.slots, j), 0, v)
    return SlotMut(ValueAt(self.t.slots, j), 0)
}

// KeySlot stores k, whose TextKeyOf is probe, and claims slot for it: an
// inline probe (up to 15 bytes) is the key itself, kept in the slot; a
// copied form, unless empty, is copied into memory from a. The key copy and
// any growth come before the caller moves its value, so a failure leaves
// the caller owning it; -1 when a is out of memory, with nothing changed.
// The owner check is made for every nonempty key, inline or not, and for
// growth, so a call with another allocator panics as it does for a copied
// key.
TextMap[V].KeySlot(mut self, a: &Alloc, hash: u64, slot: int, k: str, probe: TextKey, op: str): int {
    n := k.Len()
    if n > 0 || TablePlaceKind(&self.t, slot) == PlaceGrow {
        OwnerCheck(a, self.owner, op)
    }
    var key := probe
    copied := n > 0 && !TextKeyIsInline(&probe)
    if copied {
        p := AllocBytes(a, n, 1)
        if raw.IsNull(p) {
            return -1
        }
        CopyBytes(p, raw.StrData(k), n)
        key = TextKey(p: p, n: Trunc[u64](n))
    }
    j := TablePlace(mut self.t, a, true, hash, slot)
    if j < 0 {
        if copied {
            FreeBytes(a, key.p, n, 1)
        }
        return -1
    }
    SlotWrite(KeyAt(self.t.slots, j), 0, key)
    return j
}

/// Removes k and returns its value, or none when k is absent. The map's copy
/// of the key, if it made one, goes back to a, which must be the map's
/// allocator (code 12 otherwise). The empty key has no copy, so removing it
/// does not look at a. Expected O(1 + k.Len()).
export TextMap[V].Remove(mut self, a: &Alloc, k: str): ?V {
    p := TextProbe(&self.t, k)
    i := TableFind(&self.t, p.hash, &p.key)
    if i < 0 {
        return none
    }
    if k.Len() > 0 {
        OwnerCheck(a, self.owner, "TextMap.Remove")
    }
    v := SlotRead(ValueAt(self.t.slots, i), 0)
    FreeKey(a, SlotRead(KeyAt(self.t.slots, i), 0))
    TableErase(mut self.t, i)
    return some(v)
}

// FreeKey returns a TextMap key's copy to a; a key kept in its slot, and
// the empty key, have none.
FreeKey(a: &Alloc, key: TextKey) {
    if !TextKeyIsInline(&key) && key.n > 0 {
        FreeBytes(a, key.p, Trunc[int](key.n), 1)
    }
}

/// The number of entries. O(1).
export TextMap[V].Len(&self): int {
    return self.t.Count()
}

/// An iterator over the entries, in the map's unspecified order. O(1); a full
/// walk is O(capacity).
export TextMap[V].Iter(&self): TextMapIter[V] {
    return TextMapIter[V](map: &self.t, i: 0)
}

/// The next entry, or none after the last; allocates nothing. A full walk is
/// O(capacity): one 8-byte group read per 8 slots. A table never shrinks,
/// since removal only marks slots, so one call on a map that removals left
/// sparse may read up to capacity / 8 groups.
export TextMapIter[V].Next(mut self): ?(key: str, value: &V) from self.map {
    i := TableNext(self.map, self.i)
    if i < 0 {
        self.i = self.map.cap
        return none
    }
    self.i = i + 1
    return some((key: TextKeyStr[V](SlotRef(KeyAt(self.map.slots, i), 0)), value: SlotRef(ValueAt(self.map.slots, i), 0)))
}

/// Frees every key copy and releases every value with a, then frees the
/// table. An a that is not the map's allocator panics (code 12). A call is
/// E0701 when V is linear and not Free. The walk stops at the last full
/// slot, so an empty map walks nothing. O(capacity / 8) group reads plus the
/// values' own Free.
impl[V] Free for TextMap[V] {
    Free(self, a: &Alloc) {
        TextMap(t, owner, marker) := self
        OwnerCheck(a, owner, "TextMap.Free")
        Discard(marker)
        release := NeedsRelease[V]()
        var left := t.used
        var i := 0
        for left > 0 {
            i = TableNext(&t, i)
            FreeKey(a, SlotRead(KeyAt(t.slots, i), 0))
            if release {
                ReleaseValue(SlotRead(ValueAt(t.slots, i), 0), a)
            }
            i += 1
            left -= 1
        }
        TableFreeArrays(&t, a)
    }
}

/// Frees the table of an empty map; a must be the map's allocator (code 12
/// otherwise). Panics (code 11) when the map still holds an entry. It ends a
/// map whose V is
/// linear and not Free, after every entry is removed. O(1).
export TextMap[V].FreeEmpty(self, a: &Alloc) {
    if self.t.used > 0 {
        raw.Fail(11, "TextMap.FreeEmpty of a non-empty map")
    }
    TextMap(t, owner, marker) := self
    OwnerCheck(a, owner, "TextMap.FreeEmpty")
    Discard(marker)
    TableFreeArrays(&t, a)
}

/// Hands every value to release, in the map's iteration order, freeing each
/// key copy, then frees the table; a must be the map's allocator (code 12
/// otherwise). O(capacity / 8) group reads plus Len() calls of release.
export TextMap[V].FreeEach(self, a: &Alloc, const release: fn(V)) {
    TextMap(t, owner, marker) := self
    OwnerCheck(a, owner, "TextMap.FreeEach")
    Discard(marker)
    var left := t.used
    var i := 0
    for left > 0 {
        i = TableNext(&t, i)
        FreeKey(a, SlotRead(KeyAt(t.slots, i), 0))
        release(SlotRead(ValueAt(t.slots, i), 0))
        i += 1
        left -= 1
    }
    TableFreeArrays(&t, a)
}

// TextKey is a TextMap key in its slot, 16 bytes, the size of a str. Its
// copied form is a str's: p the key's bytes, n its length, whose top byte
// is 0. Natively a key of up to TextKeyInline bytes is inline instead: its
// bytes in order from byte 0 of the key to byte 14 (the targets are
// little-endian; bytes 0 to 7 overlay p, mem_table.mk's TextKeyWord0), every
// byte past the key zero, and byte 7 of n 0x80 | the length; so two inline
// keys are equal exactly when both words are, and the bytes in memory are
// the key's. A probe (TextKeyOf) has the same form, a copied one viewing the
// caller's bytes. A compile-time evaluation makes no inline key, since its
// memory keeps pointers and integers apart, so its keys are all copied;
// TextKeyEq compares a key of one form with one of the other by their text.
struct TextKey {
    p: *u8
    n: u64
}

// TextKeyInline is the longest key kept inline.
const TextKeyInline: int = 15

// TextKeyIsInline reports whether k is inline: the top byte of n holds the
// tag.
TextKeyIsInline(k: &TextKey): bool {
    return k.n >> 56 != 0
}

// TextKeyOf is k's TextKey: inline when k has up to 15 bytes and the code
// runs natively, read with a fixed number of loads, all inside k (FoldStr's
// short reads); else the copied form over k's own bytes. Generic over the
// map's value type only, so it is instantiated, and inlined, in the
// caller's pack (FoldStr).
TextKeyOf[V](k: str): TextKey {
    n := k.Len()
    if n > TextKeyInline || Evaluating() {
        return TextKey(p: raw.StrData(k), n: Trunc[u64](n))
    }
    var w0: u64 = 0
    var w1: u64 = 0
    if n >= 8 {
        w0 = LoadU64(k, 0)
        if n > 8 {
            w1 = LoadU64(k, n -% 8) >> ((8 *% (16 -% n)) & 63)
        }
    } else if n >= 4 {
        b := k.AsBytes()
        j := n -% 4
        lo := u64(b[0]) | (u64(b[1]) << 8) | (u64(b[2]) << 16) | (u64(b[3]) << 24)
        hi := u64(b[j]) | (u64(b[j +% 1]) << 8) | (u64(b[j +% 2]) << 16) | (u64(b[j +% 3]) << 24)
        w0 = lo | (hi << ((8 *% j) & 63))
    } else if n > 0 {
        b := k.AsBytes()
        m := n >> 1
        e := n -% 1
        w0 = u64(b[0]) | (u64(b[m]) << ((8 *% m) & 63)) | (u64(b[e]) << ((8 *% e) & 63))
    }
    return TextKeyInlineOf[V](w0, w1 | ((0x80 | Trunc[u64](n)) << 56))
}

// TextKeyStr is the text of the key at k: a view of the inline bytes in
// place, or of the copy. Either lives as long as the slot or probe k
// points at, which is what the view borrows. Generic over the key type
// only, as TextKeyOf.
TextKeyStr[K](k: &TextKey): str {
    if TextKeyIsInline(k) {
        return TextKeyInlineStr[K](k, Trunc[int]((k.n >> 56) & 0x7F))
    }
    return StrOf(k.p, Trunc[int](k.n))
}

// TextKeyEq is the comparison of every TextMap probe. Equal n words are
// equal lengths: two inline keys are equal when their first words are too
// (TextKeyWord0 is read only for an inline key), two copied ones when their
// bytes are. Unequal n words with both inline or both copied are different
// keys; a key of each form compares their text (a real table never holds
// both, since natively each length has one form and an evaluation copies
// every key; only a TextKey built directly meets this case). Generic over the key type only, as
// TextKeyOf.
TextKeyEq[K](a: &TextKey, b: &TextKey): bool {
    if a.n == b.n {
        if TextKeyIsInline(a) {
            return TextKeyWord0[K](a) == TextKeyWord0[K](b)
        }
        n := Trunc[int](a.n)
        return StrEq[K](StrOf(a.p, n), StrOf(b.p, n))
    }
    if TextKeyIsInline(a) == TextKeyIsInline(b) {
        return false
    }
    return StrEq[K](TextKeyStr[K](a), TextKeyStr[K](b))
}

// TextProbe is k's TextKey (TextKeyOf) and its hash in t, the value
// TableHash gives for k as a str key (FoldStr, or SipStrKeyed under a
// NewKeyed table's key), so a TextMap orders its keys as a HeapMap[str, V]
// with the same history would. An inline key's fold hash comes from its
// words (TextKeyFold), with no second read of k. Generic over the map's
// value type only, as TextKeyOf.
TextProbe[V](t: &Table[TextKey, V], k: str): (hash: u64, key: TextKey) {
    key := TextKeyOf[V](k)
    if t.keyed {
        return (hash: SipStrKeyed(t.k0, t.k1, k), key: key)
    }
    return (hash: TextKeyFold[V](&key), key: key)
}

// TextKeyFold is FoldStr of k's text. A copied key takes FoldStr itself;
// an inline key of n bytes is FoldStr's arithmetic on its words, which
// already hold FoldStr's reads, zero above the key: the text then 0xFF is
// n + 1 bytes, at most 16, so FoldStr's loops never run. Below 8 bytes the
// tail is the first word with the 0xFF on top (at 7 bytes it is a whole
// waiting word, which folds the same way); from 8 bytes the first word
// waits and the tail is n's key bytes with the 0xFF on top, and at 15 bytes that tail is a whole
// word, folded with the first into lane 0. Generic over the key type only, as
// TextKeyOf.
TextKeyFold[K](k: &TextKey): u64 {
    if !TextKeyIsInline(k) {
        return FoldStr[K](StrOf(k.p, Trunc[int](k.n)))
    }
    w0 := TextKeyWord0[K](k)
    n := Trunc[int]((k.n >> 56) & 0x7F)
    total := Trunc[u64](n +% 1)
    if n < 8 {
        return FoldEnd((w0 | (Trunc[u64](0xFF) << ((8 *% n) & 63))) ^ FoldF0, FoldF2 ^ total)
    }
    t := (k.n & 0x00FFFFFFFFFFFFFF) | (Trunc[u64](0xFF) << ((8 *% (n -% 8)) & 63))
    if n == 15 {
        return FoldEnd(Fold(w0 ^ FoldF0, t ^ FoldF1), FoldF2 ^ total)
    }
    return FoldEnd(w0 ^ FoldF0, t ^ FoldF2 ^ total)
}

// TextKey's Hash and Eq are its text's, so HashOf, HashKeyed and KeyEq,
// which take the TextKey branches in coll_table.mk, agree with them.
impl Hash for TextKey {
    Hash(&self, h: mut Hasher) {
        h.Bytes(TextKeyStr[TextKey](self).AsBytes())
        end: [1]u8 = [0xFF]
        h.Bytes(end[..])
    }
}

impl Eq for TextKey {
    Eq(&self, other: &TextKey): bool {
        return TextKeyEq[TextKey](self, other)
    }
}

/// An empty text that will allocate from a. O(1); allocates nothing until the
/// first Push or Set.
export HeapText.New(a: &Alloc): HeapText {
    return HeapText(ptr: DanglingBytes(), used: 0, cap: 0, owner: raw.Of(a))
}

/// Appends s. OutOfMemory when the buffer cannot grow, and then the text is
/// unchanged. Amortized O(s.Len()); a growth checks that a is the text's
/// allocator (code 12 otherwise), allocates from a and frees the old buffer.
export HeapText.Push(mut self, a: &Alloc, s: str): OutOfMemory!void {
    n := s.Len()
    if n == 0 {
        return
    }
    if n > self.cap - self.used {
        OwnerCheck(a, self.owner, "HeapText.Push")
        c := NextCap(self.cap, self.used + n)
        p := GrowBytes(a, self.ptr, self.used, self.cap, c, true)
        if raw.IsNull(p) {
            fail OutOfMemory
        }
        self.ptr = p
        self.cap = c
    }
    CopyBytes(ByteAt(self.ptr, self.used), raw.StrData(s), n)
    self.used += n
}

/// Replaces the text with s. OutOfMemory when the buffer cannot grow, and
/// then the text is unchanged. O(s.Len()); when s fits the capacity it is
/// copied in place and nothing is allocated. Otherwise the buffer grows from
/// a, which must be the text's allocator (code 12 otherwise), and the old
/// bytes are not copied.
export HeapText.Set(mut self, a: &Alloc, s: str): OutOfMemory!void {
    n := s.Len()
    if n > self.cap {
        OwnerCheck(a, self.owner, "HeapText.Set")
        c := NextCap(self.cap, n)
        p := GrowBytes(a, self.ptr, 0, self.cap, c, true)
        if raw.IsNull(p) {
            fail OutOfMemory
        }
        self.ptr = p
        self.cap = c
    }
    CopyBytes(self.ptr, raw.StrData(s), n)
    self.used = n
}

/// Empties the text and keeps its buffer for reuse. O(1).
export HeapText.Clear(mut self) {
    self.used = 0
}

/// The text's length in bytes. O(1).
export HeapText.Len(&self): int {
    return self.used
}

/// The text as a str, borrowed from the HeapText. O(1); allocates nothing.
export HeapText.Str(&self): str {
    return StrOf(self.ptr, self.used)
}

impl Free for HeapText {
    Free(self, a: &Alloc) {
        HeapText(ptr, used: _, cap, owner) := self
        OwnerCheck(a, owner, "HeapText.Free")
        if cap > 0 {
            FreeBytes(a, ptr, cap, 1)
        }
    }
}
