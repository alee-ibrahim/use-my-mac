package agent

import "testing"

func TestEffectiveMaxTasks(t *testing.T) {
	for _, c := range []struct{ own, limit, router, want int }{
		{1, 8, 0, 1},  // no router setting: agent.yaml
		{2, 8, 4, 4},  // router setting below the hard limit
		{2, 8, 20, 8}, // capped at max_tasks_limit
		{3, 8, 1, 1},  // the router may lower it too
		{0, 0, 0, 1},
	} {
		if got := EffectiveMaxTasks(c.own, c.limit, c.router); got != c.want {
			t.Errorf("EffectiveMaxTasks(%d, %d, %d) = %d, want %d", c.own, c.limit, c.router, got, c.want)
		}
	}
}
