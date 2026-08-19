package tui

import (
	"strings"
	"testing"
)

// The board is where cone is looked at all day, so a stale install has to be visible there
// and not only from `cone version`.
func TestViewShowsUpdateNotice(t *testing.T) {
	m := &model{w: 80}
	if got := m.View(); strings.Contains(got, "available") {
		t.Errorf("a current install advertised an update:\n%s", got)
	}

	updated, _ := m.Update(updateMsg{tag: "v0.2.0"})
	got := updated.View()
	if !strings.Contains(got, "v0.2.0 available") || !strings.Contains(got, "cone update") {
		t.Errorf("the update notice is missing from the status area:\n%s", got)
	}
}
