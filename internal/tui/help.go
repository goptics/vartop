package tui

import "github.com/charmbracelet/bubbles/help"

// helpFooter renders a keymap's short help using the vartop palette, truncated
// to the given width so it never wraps the footer.
func helpFooter(km help.KeyMap, width int) string {
	h := help.New()
	h.Width = width
	h.Styles.ShortKey = HelpKeyStyle
	h.Styles.ShortDesc = HelpDescStyle
	h.Styles.ShortSeparator = HelpDescStyle
	h.Styles.Ellipsis = HelpDescStyle
	return h.View(km)
}
