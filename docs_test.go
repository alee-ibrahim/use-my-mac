package usemymac

import "testing"

func TestDocsEmbedded(t *testing.T) {
	ds := Docs()
	if len(ds) != len(docOrder) {
		t.Fatalf("got %d docs, want %d (a file in docOrder is missing from the embed)", len(ds), len(docOrder))
	}
	want := map[string]string{"overview": "Overview", "usage": "Using use-my-mac", "design": "Design"}
	for _, d := range ds {
		if w, ok := want[d.Slug]; ok && d.Title != w {
			t.Errorf("%s: title %q, want %q", d.Slug, d.Title, w)
		}
		if b, ok := DocMarkdown(d.Slug); !ok || len(b) == 0 {
			t.Errorf("%s: no markdown", d.Slug)
		}
	}
	if _, ok := DocMarkdown("../go.mod"); ok {
		t.Error("unknown slug must not resolve")
	}
}

func TestTitle(t *testing.T) {
	if got := title("no heading", "fb"); got != "fb" {
		t.Errorf("fallback: %q", got)
	}
	if got := title("# Fleet install (`umm fleet`)\n", "f"); got != "Fleet install (umm fleet)" {
		t.Errorf("got %q", got)
	}
}
