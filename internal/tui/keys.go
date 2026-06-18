package tui

import "github.com/charmbracelet/bubbles/key"

// keyMap is the dashboard keymap. It implements help.KeyMap so bubbles/help can
// render the short and full help views.
type keyMap struct {
	Up      key.Binding
	Down    key.Binding
	Enter   key.Binding
	Back    key.Binding
	Section key.Binding
	PrevTab key.Binding
	Filter  key.Binding
	Search  key.Binding
	Open    key.Binding
	Help    key.Binding
	Quit    key.Binding
}

func defaultKeys() keyMap {
	return keyMap{
		Up:      key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down:    key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		Enter:   key.NewBinding(key.WithKeys("enter", "right", "l"), key.WithHelp("enter/→", "open")),
		Back:    key.NewBinding(key.WithKeys("esc", "left", "h"), key.WithHelp("esc/←", "back")),
		Section: key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "next section")),
		PrevTab: key.NewBinding(key.WithKeys("shift+tab"), key.WithHelp("shift+tab", "prev section")),
		Filter:  key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
		Search:  key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "search brain")),
		Open:    key.NewBinding(key.WithKeys("o"), key.WithHelp("o", "open source")),
		Help:    key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Quit:    key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	}
}

// ShortHelp is the single-line help shown in the footer.
func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Up, k.Down, k.Enter, k.Section, k.Filter, k.Search, k.Open, k.Help, k.Quit}
}

// FullHelp is the expanded help shown when ? is toggled.
func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Up, k.Down, k.Enter, k.Back},
		{k.Section, k.PrevTab, k.Filter, k.Search},
		{k.Open, k.Help, k.Quit},
	}
}
