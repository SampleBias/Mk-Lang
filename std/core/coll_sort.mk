// Why: a caller that names an allocator for a stable sort's scratch must get
// it from there, and must hear when it cannot.
//
// What: Slice[T].SortStable and Slice[T].SortStableIn, here in layer 1
// since both allocate.
//
// How: mem_drift.mk's DriftSort through scratch from the process heap
// (SortStable) or from the given allocator (SortStableIn). Short inputs and a
// zero-size T skip the allocation and sort in place.

/// Sorts the elements by less, keeping equal elements in their order.
/// O(n log n) calls of less and moves on every input. It allocates: past
/// 20 elements it merges through scratch memory of Len() / 2 elements or
/// more (the whole slice while that is at most 8,000,000 bytes), on the
/// stack when that fits in 4,096 bytes and T is aligned to at most 8, else
/// from the process heap, freed before it returns; through scratch, input
/// that is already sorted or strictly descending costs O(n). When its
/// scratch is larger than the stack's and the heap is out of memory, at
/// compile time or in a freestanding program, it sorts in place instead,
/// with the same result. SortStableIn takes the scratch from an allocator
/// it is given. The algorithm is Rust's driftsort, and in place a block
/// merge sort.
export Slice[T].SortStable(mut self, const less: fn(&T, &T): bool) {
    s := self.ItemsMut()
    // A short slice is one insertion sort (StableSort's and DriftStable's
    // first step), here, so it enters no frame that holds a stack scratch
    // or cache.
    if s.Len() <= DriftInsertMax {
        PdqInsertion(s, 0, s.Len(), less)
        return
    }
    DriftStable(s, less)
}

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
