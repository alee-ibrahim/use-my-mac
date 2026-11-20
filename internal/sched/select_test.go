package sched

import (
	"testing"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

func readyCand(id, name string) Candidate {
	return Candidate{ID: id, Name: name, Online: true, AgentState: api.AgentReady, Runtimes: []string{"sh"}, MaxTasks: 1,
		Telemetry: api.Telemetry{OnAC: true, CPUCount: 8}}
}

func TestReservationPinAndPrefer(t *testing.T) {
	w, l := DefaultWeights(), DefaultLimits()
	res := readyCand("m1", "studio")
	res.ReservedFor = []string{"key:a"}
	free := readyCand("m2", "lab")
	free.Labels = map[string]string{"class": "studio"}

	if r := Score(res, Need{Runtime: "sh", Owner: "key:b"}, w, l); r.OK || r.Reason != ReasonReserved {
		t.Fatalf("other owner on reserved Mac: %+v", r)
	}
	if r := Score(res, Need{Runtime: "sh", Owner: "key:a"}, w, l); !r.OK {
		t.Fatalf("owner on reserved Mac: %+v", r)
	}
	if r := Score(free, Need{Runtime: "sh", Macs: []string{"studio"}}, w, l); r.OK || r.Reason != ReasonNotPinned {
		t.Fatalf("pin by name: %+v", r)
	}
	if r := Score(res, Need{Runtime: "sh", Owner: "key:a", Macs: []string{"m1"}}, w, l); !r.OK {
		t.Fatalf("pin by ID: %+v", r)
	}
	plain := Score(free, Need{Runtime: "sh"}, w, l)
	pref := Score(free, Need{Runtime: "sh", Prefer: &api.Prefer{Labels: map[string]string{"class": "studio"}}}, w, l)
	if pref.Score != plain.Score+DefaultPreferBonus || pref.Parts["prefer"] != 1 {
		t.Fatalf("prefer bonus: %v vs %v", pref.Score, plain.Score)
	}
	other := Score(res, Need{Runtime: "sh", Owner: "key:a", Prefer: &api.Prefer{Macs: []string{"lab"}, Bonus: 5}}, w, l)
	if !other.OK || other.Parts["prefer"] != 0 {
		t.Fatalf("non-preferred Mac still OK without bonus: %+v", other)
	}
}
