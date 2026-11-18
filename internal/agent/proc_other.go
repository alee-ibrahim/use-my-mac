//go:build !darwin

package agent

// setGroupBackground is a no-op outside macOS (adaptive priority only
// changes anything there).
func setGroupBackground(pgid int, bg bool) error { return nil }
