// Why: LowerInternal may differ from the C ABI only where it means to: a
// Win64 two-INTEGER aggregate of 9 to 16 bytes.
// What: TestLowerInternal over every non-variadic row of the suite.
// How: each target's LowerInternal is compared with its Lower slot by slot;
// a Win64 slot that differs must be Lower's Indirect slot as System V
// classifies the same type, two INTEGER parts.

package abi

import (
	"reflect"
	"testing"

	"mk/internal/types"
)

func TestLowerInternal(t *testing.T) {
	w := loadSuite(t)
	changed := 0
	for _, target := range types.Targets() {
		for _, r := range w.rows {
			if r.variadic {
				continue
			}
			c := Lower(w.tt, w.layout, target, r.params, r.fixed, false, r.result)
			in := LowerInternal(w.tt, w.layout, target, r.params, r.result)
			slots := append([]Slot{c.Ret}, c.Params...)
			islots := append([]Slot{in.Ret}, in.Params...)
			types_ := append([]types.TypeID{r.result}, r.params...)
			for i := range slots {
				if reflect.DeepEqual(slots[i], islots[i]) {
					continue
				}
				if target != types.X86_64Windows {
					t.Errorf("%s %s slot %d: internal %+v, C %+v: only Win64 may differ", target.Name, r.name, i, islots[i], slots[i])
					continue
				}
				sv := sysvClassify(newView(w.tt, w.layout, types.X86_64Linux), types_[i])
				ok := slots[i].Kind == Indirect && slots[i].Size > 8 && slots[i].Size <= 16 && islots[i].Kind == Direct &&
					len(islots[i].Parts) == 2 && !sv.memory && sv.needSSE == 0 && reflect.DeepEqual(islots[i], sv.slot)
				for _, p := range islots[i].Parts {
					ok = ok && p.Class == types.RegInt
				}
				if !ok {
					t.Errorf("Win64 %s slot %d: internal %+v, C %+v, System V %+v", r.name, i, islots[i], slots[i], sv.slot)
				}
				changed++
			}
		}
	}
	if changed == 0 {
		t.Error("no suite row changed on Win64: the suite has no 16-byte integer pair")
	}
}
