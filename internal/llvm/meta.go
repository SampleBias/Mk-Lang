// Why: metadata (debug locations, module flags, branch weights, load facts)
// must print in a stable order and must never name a node of another
// module.
//
// What: MD and Meta, the module's metadata table, with ModuleFlag, the
// line-table nodes (DICompileUnit, DIFile, DISubprogram, DILocation),
// DebugFlags, !prof branch weights and the load facts (!range, !nonnull,
// !noundef, !align).
//
// How: nodes are numbered in creation order and printed sorted by number
// after the functions. Locations, the one node kind made per statement, are
// kept as numbers and printed by WriteTo; an MD remembers its table, so a
// node used in the wrong module panics.

package llvm

import "strconv"

// MD is a metadata node of one Module, printed !<n>. The zero MD is "none":
// an attachment of the zero MD is not printed, and as an operand it prints
// as null. An MD remembers its Module's metadata table, so using it in
// another Module panics instead of naming that Module's node of the same
// number.
type MD struct {
	id uint32
	mt *Meta
}

// IsZero reports whether md is "none".
func (md MD) IsZero() bool { return md.id == 0 }

func (md MD) ref() string {
	if md.id == 0 {
		return "null"
	}
	return "!" + strconv.FormatUint(uint64(md.id-1), 10)
}

// MDOp is one operand of a metadata tuple. It keeps the node or value it
// refers to, so the Meta it is used in can check that both belong to it.
type MDOp struct {
	text string
	md   MD
	v    Value
}

// MDStr is the metadata string !"s".
func MDStr(s string) MDOp { return MDOp{text: "!" + quoted(s)} }

// MDVal is a constant value operand, "i32 1" or "ptr @f".
func MDVal(v Value) MDOp { return MDOp{text: v.String(), v: v} }

// MDRef is a reference to another node (null for the zero MD).
func MDRef(md MD) MDOp { return MDOp{text: md.ref(), md: md} }

// MDNull is the null operand.
func MDNull() MDOp { return MDOp{text: "null"} }

// Field is one field of a specialized node, key: value.
type Field struct {
	key, text string
	md        MD
}

// FInt is an integer field, line: 3.
func FInt(key string, n int64) Field { return Field{key: key, text: strconv.FormatInt(n, 10)} }

// FUint is an unsigned integer field, value: 18446744073709551615.
func FUint(key string, n uint64) Field { return Field{key: key, text: strconv.FormatUint(n, 10)} }

// FStr is a string field, producer: "mk".
func FStr(key, s string) Field { return Field{key: key, text: quoted(s)} }

// FRef is a node field, file: !1 (null for the zero MD).
func FRef(key string, md MD) Field { return Field{key: key, text: md.ref(), md: md} }

// FEnum is a keyword field, emissionKind: LineTablesOnly.
func FEnum(key, ident string) Field { return Field{key: key, text: ident} }

// FBool is a boolean field, isOptimized: true.
func FBool(key string, b bool) Field { return Field{key: key, text: strconv.FormatBool(b)} }

// Attach is a metadata attachment of an instruction: !dbg, !prof, !range,
// !nonnull, !noundef, !align. One of a zero MD is not printed.
type Attach struct {
	Kind string
	MD   MD
}

// With is the attachment !kind !md.
func With(kind string, md MD) Attach { return Attach{kind, md} }

// Dbg is the attachment !dbg (a DILocation).
func Dbg(md MD) Attach { return Attach{"dbg", md} }

// Prof is the attachment !prof (branch_weights).
func Prof(md MD) Attach { return Attach{"prof", md} }

// Range is the attachment !range (a node from Meta.Range).
func Range(md MD) Attach { return Attach{"range", md} }

type namedMD struct {
	name  string
	nodes []MD
}

// Meta is a module's metadata. Nodes are numbered in creation order;
// uniqued nodes (tuples and non-distinct specialized nodes) with equal text
// are one node; distinct nodes are always new.
type Meta struct {
	m     *Module
	nodes []mdNode // node id-1
	uniq  map[string]MD
	locs  map[locKey]MD // the uniqued DILocations
	sc    []byte        // scratch for node text; a uniqued hit allocates nothing
	sc2   []byte        // scratch for "distinct " + sc
	subr  MD            // the empty DISubroutineType
	empty MD            // !{}
	named []namedMD
	flags []MD
	fkeys map[string]MD // module flag key -> its node
	cu    MD
}

// mdNode is one numbered node: its text after "!n = ", or, when text is
// empty, the DILocation loc. Locations are the one node kind made per
// statement, so they are kept as numbers and printed by WriteTo.
type mdNode struct {
	text string
	loc  locKey
	// open marks a node Reserve made and Fill has not filled (dbg.go).
	open bool
}

type locKey struct{ line, col, scope uint32 }

// appendNode appends the text of node i (0-based).
func (mt *Meta) appendNode(buf []byte, i int) []byte {
	n := &mt.nodes[i]
	if n.text != "" {
		return append(buf, n.text...)
	}
	buf = append(buf, "!DILocation(line: "...)
	buf = strconv.AppendUint(buf, uint64(n.loc.line), 10)
	buf = append(buf, ", column: "...)
	buf = strconv.AppendUint(buf, uint64(n.loc.col), 10)
	buf = append(buf, ", scope: "...)
	buf = appendMDRef(buf, MD{n.loc.scope, mt})
	return append(buf, ')')
}

// check panics unless md is the zero MD or a node of this Meta.
func (mt *Meta) check(md MD, what string) {
	if md.id == 0 {
		return
	}
	if md.mt != mt || int(md.id) > len(mt.nodes) {
		panic("llvm: " + what + ": metadata node from another module")
	}
}

func (mt *Meta) checkOps(ops []MDOp, what string) {
	for _, o := range ops {
		mt.check(o.md, what)
		if !o.v.IsZero() {
			mt.m.checkValue(o.v, what)
		}
	}
}

func (mt *Meta) checkFields(fs []Field, what string) {
	for _, f := range fs {
		mt.check(f.md, what)
	}
}

// add makes the node whose text is mt.sc: a new one when distinct, else the
// existing node of equal text or a new one.
func (mt *Meta) add(distinct bool) MD {
	if distinct {
		mt.sc2 = append(append(mt.sc2[:0], "distinct "...), mt.sc...)
		mt.nodes = append(mt.nodes, mdNode{text: string(mt.sc2)})
		return MD{uint32(len(mt.nodes)), mt}
	}
	if md, ok := mt.uniq[string(mt.sc)]; ok { // no allocation on a hit
		return md
	}
	text := string(mt.sc)
	mt.nodes = append(mt.nodes, mdNode{text: text})
	md := MD{uint32(len(mt.nodes)), mt}
	mt.uniq[text] = md
	return md
}

func (mt *Meta) tupleText(ops []MDOp) {
	b := append(mt.sc[:0], "!{"...)
	for i, o := range ops {
		if i > 0 {
			b = append(b, ", "...)
		}
		b = append(b, o.text...)
	}
	mt.sc = append(b, '}')
}

// Tuple is the uniqued node !{ops...}.
func (mt *Meta) Tuple(ops ...MDOp) MD {
	mt.checkOps(ops, "Tuple")
	mt.tupleText(ops)
	return mt.add(false)
}

// DistinctTuple is a new distinct !{ops...}.
func (mt *Meta) DistinctTuple(ops ...MDOp) MD {
	mt.checkOps(ops, "DistinctTuple")
	mt.tupleText(ops)
	return mt.add(true)
}

func (mt *Meta) nodeText(kind string, fields []Field) {
	b := append(mt.sc[:0], '!')
	b = append(b, kind...)
	b = append(b, '(')
	for i, f := range fields {
		if i > 0 {
			b = append(b, ", "...)
		}
		b = append(b, f.key...)
		b = append(b, ": "...)
		b = append(b, f.text...)
	}
	mt.sc = append(b, ')')
}

// Node is the uniqued specialized node !kind(fields...), e.g. !DIFile.
func (mt *Meta) Node(kind string, fields ...Field) MD {
	mt.checkFields(fields, "Node")
	mt.nodeText(kind, fields)
	return mt.add(false)
}

// DistinctNode is a new distinct specialized node.
func (mt *Meta) DistinctNode(kind string, fields ...Field) MD {
	mt.checkFields(fields, "DistinctNode")
	mt.nodeText(kind, fields)
	return mt.add(true)
}

// Named appends nodes to the named metadata !name (created on first use;
// printed in creation order).
func (mt *Meta) Named(name string, nodes ...MD) {
	for _, md := range nodes {
		mt.check(md, "Named !"+name)
	}
	for i := range mt.named {
		if mt.named[i].name == name {
			mt.named[i].nodes = append(mt.named[i].nodes, nodes...)
			return
		}
	}
	mt.named = append(mt.named, namedMD{name, append([]MD(nil), nodes...)})
}

// Module flag behaviours of !llvm.module.flags.
const (
	FlagError    = 1
	FlagWarning  = 2
	FlagOverride = 4
	FlagMax      = 7
)

// ModuleFlag adds !{i32 behavior, !"key", i32 value} to !llvm.module.flags.
// LLVM's verifier rejects two flags with one key (clang then aborts with a
// backend fatal error), so each key is printed once: adding the same flag
// again is a no-op, and the same key with another behaviour or value
// panics.
func (mt *Meta) ModuleFlag(behavior int, key string, value int64) {
	i32 := mt.m.I(32)
	md := mt.Tuple(MDVal(mt.m.ConstInt(i32, uint64(behavior))), MDStr(key),
		MDVal(mt.m.ConstInt(i32, uint64(value))))
	if old, ok := mt.fkeys[key]; ok {
		if old != md {
			panic("llvm: ModuleFlag " + strconv.Quote(key) + " added twice with different values")
		}
		return
	}
	if mt.fkeys == nil {
		mt.fkeys = map[string]MD{}
	}
	mt.fkeys[key] = md
	mt.flags = append(mt.flags, md)
}

// ModuleFlagMD adds !{i32 behavior, !"key", !value} to !llvm.module.flags
// (ProfileSummary's form), under ModuleFlag's rule: each key once, the same
// flag again a no-op, another value for the key a panic.
func (mt *Meta) ModuleFlagMD(behavior int, key string, value MD) {
	mt.check(value, "ModuleFlagMD")
	md := mt.Tuple(MDVal(mt.m.ConstInt(mt.m.I(32), uint64(behavior))), MDStr(key), MDRef(value))
	if old, ok := mt.fkeys[key]; ok {
		if old != md {
			panic("llvm: ModuleFlagMD " + strconv.Quote(key) + " added twice with different values")
		}
		return
	}
	if mt.fkeys == nil {
		mt.fkeys = map[string]MD{}
	}
	mt.fkeys[key] = md
	mt.flags = append(mt.flags, md)
}

// Empty is !{}, the operand of !nonnull and !noundef.
func (mt *Meta) Empty() MD {
	if mt.empty.IsZero() {
		mt.empty = mt.Tuple()
	}
	return mt.empty
}

// NonNull is the attachment !nonnull !{}.
func (mt *Meta) NonNull() Attach { return Attach{"nonnull", mt.Empty()} }

// NoUndef is the attachment !noundef !{}.
func (mt *Meta) NoUndef() Attach { return Attach{"noundef", mt.Empty()} }

// Unpredictable is the attachment !unpredictable !{} of a select or a
// branch: its condition follows no pattern, so the backend keeps a select a
// conditional move (x86's cmov converter skips it).
func (mt *Meta) Unpredictable() Attach { return Attach{"unpredictable", mt.Empty()} }

// Align is the attachment !align !{i64 n} for a load of a pointer.
func (mt *Meta) Align(n uint64) Attach {
	b := append(mt.sc[:0], "!{i64 "...)
	b = strconv.AppendInt(b, int64(n), 10)
	mt.sc = append(b, '}')
	return Attach{"align", mt.add(false)}
}

// BranchWeights is !{!"branch_weights", i32 w...}; br takes true then
// false, switch the default then the cases in order.
func (mt *Meta) BranchWeights(ws ...uint32) MD {
	b := append(mt.sc[:0], `!{!"branch_weights"`...)
	for _, w := range ws {
		b = append(b, ", i32 "...)
		b = strconv.AppendUint(b, uint64(w), 10)
	}
	mt.sc = append(b, '}')
	return mt.add(false)
}

// EntryCount is !{!"function_entry_count", i64 n}, for Function.SetProf.
func (mt *Meta) EntryCount(n uint64) MD {
	b := append(mt.sc[:0], `!{!"function_entry_count", i64 `...)
	b = strconv.AppendUint(b, n, 10)
	mt.sc = append(b, '}')
	return mt.add(false)
}

// Range is the !range node of the half-open pairs [lo, hi) of an integer
// type t; pairs is flat (lo0, hi0, lo1, hi1, ...) and already normalized
// (RangePairs). An empty pairs is the zero MD: no !range.
func (mt *Meta) Range(t Type, pairs []int64) MD {
	if len(pairs) == 0 {
		return MD{}
	}
	mt.m.checkType(t, "Range")
	if len(pairs)%2 != 0 || !t.IsInt() {
		panic("llvm: Range needs an integer type and an even number of bounds")
	}
	b := append(mt.sc[:0], "!{"...)
	for i, p := range pairs {
		if i > 0 {
			b = append(b, ", "...)
		}
		b = appendTyped(b, mt.m.ConstInt(t, uint64(p)))
	}
	mt.sc = append(b, '}')
	return mt.add(false)
}

// DIFile is !DIFile(filename, directory), with the package-relative path
// and directory ".".
func (mt *Meta) DIFile(filename, directory string) MD {
	return mt.Node("DIFile", FStr("filename", filename), FStr("directory", directory))
}

// DICompileUnit is the pack's distinct line-tables-only compile unit
// (producer "mk", DW_LANG_C99), added to !llvm.dbg.cu.
func (mt *Meta) DICompileUnit(file MD, optimized bool) MD {
	cu := mt.DistinctNode("DICompileUnit", FEnum("language", "DW_LANG_C99"), FRef("file", file),
		FStr("producer", "mk"), FBool("isOptimized", optimized), FInt("runtimeVersion", 0),
		FEnum("emissionKind", "LineTablesOnly"))
	mt.Named("llvm.dbg.cu", cu)
	mt.cu = cu
	return cu
}

// DISubroutineType is the empty subroutine type of line-tables-only info.
func (mt *Meta) DISubroutineType() MD {
	if mt.subr.IsZero() {
		mt.subr = mt.Node("DISubroutineType", FRef("types", mt.Empty()))
	}
	return mt.subr
}

// DISubprogram is a function definition's distinct subprogram.
func (mt *Meta) DISubprogram(name, linkageName string, file MD, line int, unit MD, optimized bool) MD {
	mt.check(file, "DISubprogram")
	mt.check(unit, "DISubprogram")
	ty := mt.DISubroutineType()
	// One per function: written straight into the scratch, in the field
	// order of Node's text.
	b := append(mt.sc[:0], "!DISubprogram(name: "...)
	b = appendQuoted(b, name)
	if linkageName != "" && linkageName != name {
		b = append(b, ", linkageName: "...)
		b = appendQuoted(b, linkageName)
	}
	b = append(b, ", scope: "...)
	b = appendMDRef(b, file)
	b = append(b, ", file: "...)
	b = appendMDRef(b, file)
	b = append(b, ", line: "...)
	b = strconv.AppendInt(b, int64(line), 10)
	b = append(b, ", type: "...)
	b = appendMDRef(b, ty)
	b = append(b, ", scopeLine: "...)
	b = strconv.AppendInt(b, int64(line), 10)
	b = append(b, ", spFlags: DISPFlagDefinition"...)
	if optimized {
		b = append(b, " | DISPFlagOptimized"...)
	}
	b = append(b, ", unit: "...)
	b = appendMDRef(b, unit)
	mt.sc = append(b, ')')
	return mt.add(true)
}

// MaxColumn is the largest column LLVM's parser accepts in a DILocation
// (LLVM 18 to 21: "value for 'column' too large, limit is 65535").
const MaxColumn = 65535

// DILocation is the uniqued location line:col in scope. A column above
// MaxColumn (a source line longer than 64 KiB) is printed as 0, LLVM's
// "column unknown", so the line table degrades instead of clang rejecting
// the pack; a negative column is 0 too. The line is clamped to
// 0..2^32-1, the range of its field.
func (mt *Meta) DILocation(line, col int, scope MD) MD {
	if col < 0 || col > MaxColumn {
		col = 0
	}
	l := int64(line)
	if l < 0 {
		l = 0
	} else if l > 1<<32-1 {
		l = 1<<32 - 1
	}
	mt.check(scope, "DILocation")
	key := locKey{uint32(l), uint32(col), scope.id}
	if md, ok := mt.locs[key]; ok {
		return md
	}
	if mt.locs == nil {
		mt.locs = map[locKey]MD{}
	}
	mt.nodes = append(mt.nodes, mdNode{loc: key})
	md := MD{uint32(len(mt.nodes)), mt}
	mt.locs[key] = md
	return md
}

// DebugFlags adds the module flags of line tables: Debug Info Version 3,
// and CodeView 1 for COFF or Dwarf Version 4 otherwise.
func (mt *Meta) DebugFlags(coff bool) {
	if coff {
		mt.ModuleFlag(FlagWarning, "CodeView", 1)
	} else {
		mt.ModuleFlag(FlagMax, "Dwarf Version", 4)
	}
	mt.ModuleFlag(FlagWarning, "Debug Info Version", 3)
}
