// Package usemymac embeds the project's user-facing documentation so the
// router can serve it at /docs without anything extra on disk.
package usemymac

import (
	"embed"
	"strings"
)

//go:embed README.md docs/*.md
var docFS embed.FS

// Doc is one embedded Markdown page.
type Doc struct {
	Slug  string `json:"slug"`
	Title string `json:"title"`
	// Path is the file's path in the repository, so relative links can be
	// resolved by the renderer.
	Path string `json:"path"`
}

// docOrder is the navigation order. Each entry must exist in docFS (checked by
// tests).
var docOrder = []Doc{
	{Slug: "overview", Title: "Overview", Path: "README.md"},
	{Slug: "usage", Path: "docs/usage.md"},
	{Slug: "design", Path: "docs/design.md"},
	{Slug: "fleet", Path: "docs/fleet.md"},
}

// Docs lists the embedded pages in navigation order, with titles taken from
// each file's first level-1 heading unless set in docOrder.
func Docs() []Doc {
	out := make([]Doc, 0, len(docOrder))
	for _, d := range docOrder {
		b, err := docFS.ReadFile(d.Path)
		if err != nil {
			continue
		}
		if d.Title == "" {
			d.Title = title(string(b), d.Slug)
		}
		out = append(out, d)
	}
	return out
}

// DocMarkdown returns the Markdown source of a page by slug.
func DocMarkdown(slug string) ([]byte, bool) {
	for _, d := range docOrder {
		if d.Slug == slug {
			b, err := docFS.ReadFile(d.Path)
			return b, err == nil
		}
	}
	return nil, false
}

func title(md, fallback string) string {
	for _, line := range strings.Split(md, "\n") {
		if t, ok := strings.CutPrefix(strings.TrimSpace(line), "# "); ok {
			return strings.ReplaceAll(strings.TrimSpace(t), "`", "")
		}
	}
	return fallback
}
