package tui

import "github.com/rossoctl/cortex/cmd/agentop/tui/table"

// setCursorVisible moves a table's cursor to row n, clamped to the row range, and
// leaves an empty table alone rather than parking it on a row that does not exist.
//
// The table keeps the cursor on screen by itself now, scrolling only as far as it must
// and not at all when the cursor is already on row n — see the table package's doc for
// why that took a copy of bubbles' table. That last property is what the two-second
// poll leans on: the panes that call this restore their cursor on every poll, so on a
// live session the common case by far is "the cursor is already on row n", and the
// restore must leave a reader's scroll position exactly where it was.
//
// The empty case is the one thing SetCursor does not do here. A table emptied by
// SetRows(nil) parks its cursor at −1 while a fresh one sits at 0, and neither index
// addresses a row, so this declines to move rather than picking one — SetCursor would
// clamp(n, 0, −1) a fresh table's cursor off row 0.
func setCursorVisible(t *table.Model, n int) {
	if len(t.Rows()) == 0 {
		return
	}
	t.SetCursor(n)
}
