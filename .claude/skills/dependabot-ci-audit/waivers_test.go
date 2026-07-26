package audit

import (
	"slices"
	"testing"
)

func TestParseWaivers(t *testing.T) {
	// CRLF is the case that matters: the file is edited on Windows, and a
	// trailing \r makes "name\r" != "name", waiving nothing while looking right.
	text := "# comment\r\n\r\nalpha\r\nbeta # trailing comment\r\n   \r\ngamma\nalpha\n"

	w := ParseWaivers(text)

	if want := []string{"alpha", "beta", "gamma"}; !slices.Equal(w.Names(), want) {
		t.Errorf("Names() = %v, want %v", w.Names(), want)
	}
	if w.Len() != 3 {
		t.Errorf("Len() = %d, want 3 (duplicates collapsed)", w.Len())
	}
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if !w.Has(name) {
			t.Errorf("Has(%q) = false", name)
		}
	}
	if w.Has("comment") {
		t.Error("a comment line must not become a waiver")
	}
	if w.Has("delta") {
		t.Error("Has() matched a repo that was never listed")
	}
}

func TestParseWaiversEmpty(t *testing.T) {
	// -no-waivers must waive nothing rather than everything.
	w := ParseWaivers("")
	if w.Len() != 0 || w.Has("anything") {
		t.Error("an empty list must waive nothing")
	}
}

// The shipped list is what suppresses findings in real runs, so its contents are
// asserted rather than assumed to have survived edits.
func TestDefaultWaiverListParses(t *testing.T) {
	w := ParseWaivers(DefaultWaiverList)
	if w.Len() == 0 {
		t.Fatal("the compiled-in waiver list is empty")
	}
	for _, name := range w.Names() {
		if name == "" || name[0] == '#' {
			t.Errorf("parsed %q as a repo name", name)
		}
	}
}
