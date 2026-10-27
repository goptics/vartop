package tui

import "github.com/charmbracelet/bubbles/help"

// helpFooter renders a keymap's short help using the vartop palette, truncated
// to the given width so it never wraps the footer. A width under 1 — a
// screen too narrow for any — truncates to 1, never to none: help takes 0
// for no limit, and the whole help wrapped in a narrow footer would make it
// taller than the screen.
func helpFooter(km help.KeyMap, width int) string {
	h := help.New()
	h.Width = max(1, width)
	h.Styles.ShortKey = HelpKeyStyle
	h.Styles.ShortDesc = HelpDescStyle
	h.Styles.ShortSeparator = HelpDescStyle
	h.Styles.Ellipsis = HelpDescStyle
	return h.View(km)
}
