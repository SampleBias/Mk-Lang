// Why: a caller that names an allocator for a stable sort's scratch must get
// it from there, and must hear when it cannot.
//
// What: Slice[T].SortStableIn.
//
// How: core_drift.mk's DriftSort through scratch from the given allocator,
// as SortStable does through the process heap's. Short inputs and a
// zero-size T skip the allocation and sort in place.

/// Sorts the elements by less, keeping equal elements in their order, as
/// SortStable does, with its scratch memory from a instead of the process
/// heap: Len() / 2 elements, freed before it returns. O(n log n) calls of less and
/// moves, O(n) on input that is already sorted or strictly descending.
/// Below 32 elements, and for a
/// zero-size T, it sorts in place and allocates nothing. Fails with
/// OutOfMemory when the scratch cannot be allocated, before any element
/// moves, so the slice is unchanged.
export Slice[T].SortStableIn(mut self, a: &Alloc, const less: fn(&T, &T): bool): OutOfMemory!void {
    s := self.ItemsMut()
    n := s.Len()
    if n < 2 * BlkRun || SizeOfT[T]() == 0 {
        StableSort(s, less)
        return
    }
    // Half the slice, rounded down: the least DriftSort takes.
    if !DriftIn(s, a, n / 2, less) {
        fail OutOfMemory
    }
}
