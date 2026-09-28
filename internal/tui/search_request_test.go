package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"net/url"
	"strings"
	"testing"
)

func TestFileURLPreservesReservedFilenameCharacters(t *testing.T) {
	for _, path := range []string{"/work/a#b.go", "/work/a?b.go", "/work/a%20b.go", "/work/雪 file.go", `C:\work\a#b.go`} {
		u, err := url.Parse(fileURL(path))
		want := strings.ReplaceAll(path, `\`, "/")
		if !strings.HasPrefix(want, "/") {
			want = "/" + want
		}
		if err != nil || u.Path != want || u.RawQuery != "" || u.Fragment != "" {
			t.Errorf("fileURL(%q) = %#v, err=%v", path, u, err)
		}
	}
}

func submitSearch(t *testing.T, m Model, query string) (Model, tea.Cmd) {
	t.Helper()
	u, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	m = u.(Model)
	m.search.SetValue(query)
	u, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("missing search command")
	}
	return u.(Model), cmd
}

func TestSearchCompletionPreservesNewEditor(t *testing.T) {
	m := newRunModel(sampleSnapshot(), themes["default"], func(string) ([]SearchResult, error) { return nil, nil }, TabHome)
	m, cmd := submitSearch(t, m, "first")
	u, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	m = u.(Model)
	m.search.SetValue("second")
	u, _ = m.Update(cmd())
	m = u.(Model)
	if !m.searching || !m.search.Focused() || m.search.Value() != "second" {
		t.Fatalf("completion disrupted newer editor: searching=%v focused=%v value=%q", m.searching, m.search.Focused(), m.search.Value())
	}
}

func TestSameTextSearchUsesRequestIdentity(t *testing.T) {
	m := newRunModel(sampleSnapshot(), themes["default"], func(string) ([]SearchResult, error) { return nil, nil }, TabHome)
	m, first := submitSearch(t, m, "same")
	m, second := submitSearch(t, m, "same")
	old := first().(searchResultMsg)
	old.results = []SearchResult{{ID: "older"}}
	latest := second().(searchResultMsg)
	latest.results = []SearchResult{{ID: "newer"}}
	u, _ := m.Update(latest)
	m = u.(Model)
	u, _ = m.Update(old)
	m = u.(Model)
	if len(m.snap.Search) != 1 || m.snap.Search[0].ID != "newer" {
		t.Fatalf("stale result replaced latest: %+v", m.snap.Search)
	}
}
