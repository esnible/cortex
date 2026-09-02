package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/authbridge/cmd/abctl/edit"
)

// editPhase tracks where the edit state machine currently sits.
type editPhase int

const (
	editPhaseDone editPhase = iota // not editing
	editPhaseFetching
	editPhaseEditing // $EDITOR is running; bubbletea is suspended
	editPhaseValidating
	editPhaseDiff
	editPhaseApplying
	editPhaseWaiting
	editPhaseRollback // re-applying the original ConfigMap after a failed reload
	// editPhaseBackground means: user pressed Esc during Waiting/Rollback;
	// the in-flight Cmd is still running and we want to flash its result
	// in the footer rather than reopen the overlay. Overlay renders nothing
	// in this phase. fetched/applyTime stay populated so the PolledMsg
	// handler can still trigger rollback if the in-pod reload failed.
	editPhaseBackground
	editPhaseError
)

// editState lives on *model when an edit is in flight.
type editState struct {
	phase     editPhase
	fetched   *edit.FetchedPipeline
	tempPath  string
	editedRaw []byte // bytes the user wrote in $EDITOR
	diff      string // colorized output from edit.Diff
	err       string // single-line message in editPhaseError
	applyTime time.Time
	// validationErrs are dependency/claim issues abctl detected before
	// apply by checking the proposed pipeline against the plugin
	// catalog. Empty when validation passed or the catalog isn't loaded.
	// Rendered above the diff in the editPhaseDiff overlay so operators
	// see them before deciding to apply. Non-blocking — the framework's
	// own validateRelationships is still the source of truth at reload.
	validationErrs []edit.ValidationError
	// generation is bumped each time a fresh edit cycle begins (initial
	// `e`, retry from error, restart after abort). Each tea.Cmd captures
	// the value at issue time; handlers drop messages whose captured
	// generation doesn't match the current one. Without this, a late
	// PolledMsg from Edit 1 arriving after the user has Esc'd and
	// started Edit 2 would route Edit 1's reload result onto Edit 2's
	// overlay (same phase, different transaction).
	generation int
}

// renderEditOverlay returns the overlay content (rendered into a
// styled box) for the current edit phase. width/height are the
// terminal's full dimensions; the overlay sizes itself to fit
// comfortably inside.
func renderEditOverlay(s editState, width, height int) string {
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		Padding(1, 2).
		Width(min(width-4, 100))

	var b strings.Builder
	switch s.phase {
	case editPhaseFetching:
		b.WriteString(styleTitle.Render("Edit pipeline"))
		b.WriteString("\n\n")
		b.WriteString("Fetching ConfigMap…")
	case editPhaseEditing:
		b.WriteString(styleTitle.Render("Edit pipeline"))
		b.WriteString("\n\n")
		b.WriteString(fmt.Sprintf("Editor open at %s", s.tempPath))
	case editPhaseValidating:
		b.WriteString(styleTitle.Render("Edit pipeline"))
		b.WriteString("\n\n")
		b.WriteString("Validating YAML…")
	case editPhaseDiff:
		b.WriteString(styleTitle.Render("Edit pipeline — review diff"))
		b.WriteString("\n\n")
		// Validation banner: render BEFORE the diff so operators see
		// dependency issues at first glance. Non-blocking — apply still
		// works.
		//
		// Errors and warnings get separate banners because they make
		// different promises: an error means the framework's reload WILL
		// reject the pipeline, while a warning (a plugin in a chain it
		// doesn't declare) reloads fine and merely looks wrong. Folding
		// the two together would make the "will reject" line false.
		vErrs, vWarns := splitBySeverity(s.validationErrs)
		if len(vErrs) > 0 {
			b.WriteString(styleError.Render(fmt.Sprintf(
				"⚠ %d validation issue%s — framework reload will reject:",
				len(vErrs), plural(len(vErrs)))))
			b.WriteString("\n")
			writeValidationLines(&b, vErrs)
			b.WriteString("\n")
		}
		if len(vWarns) > 0 {
			b.WriteString(styleWarn.Render(fmt.Sprintf(
				"%d advisory — reload will accept, but check:",
				len(vWarns))))
			b.WriteString("\n")
			writeValidationLines(&b, vWarns)
			b.WriteString("\n")
		}
		b.WriteString(s.diff)
		b.WriteString("\n")
		// "anyway" is warranted only when the framework will actually
		// reject; an advisory doesn't change what apply does.
		if len(vErrs) > 0 {
			b.WriteString(styleHint.Render("apply anyway? (y/N)"))
		} else {
			b.WriteString(styleHint.Render("apply this change? (y/N)"))
		}
	case editPhaseApplying:
		b.WriteString(styleTitle.Render("Edit pipeline"))
		b.WriteString("\n\n")
		b.WriteString("Applying to ConfigMap…")
	case editPhaseWaiting:
		b.WriteString(styleTitle.Render("Edit pipeline"))
		b.WriteString("\n\n")
		b.WriteString("Waiting for hot-reload…")
		b.WriteString("\n")
		b.WriteString(styleHint.Render("(this can take up to 120s while kubelet syncs the ConfigMap)"))
	case editPhaseRollback:
		b.WriteString(styleTitle.Render("Edit pipeline — rolling back"))
		b.WriteString("\n\n")
		b.WriteString("Reload failed. Restoring previous ConfigMap…")
	case editPhaseError:
		b.WriteString(styleTitle.Render("Edit pipeline — error"))
		b.WriteString("\n\n")
		b.WriteString(s.err)
		b.WriteString("\n\n")
		b.WriteString(styleHint.Render("[r] re-edit  [Esc] back to Pipeline"))
	}
	return box.Render(b.String())
}

// splitBySeverity partitions validation results into hard errors (the
// framework will reject) and advisories (it will accept). Order within
// each group is preserved so the chain/position sequence still reads
// top-to-bottom.
func splitBySeverity(in []edit.ValidationError) (errs, warns []edit.ValidationError) {
	for _, ve := range in {
		if ve.Severity == edit.SeverityWarning {
			warns = append(warns, ve)
		} else {
			errs = append(errs, ve)
		}
	}
	return errs, warns
}

// writeValidationLines renders one bullet per validation result.
func writeValidationLines(b *strings.Builder, in []edit.ValidationError) {
	for _, ve := range in {
		b.WriteString(fmt.Sprintf("  • [%s] %s pos %d: %s\n",
			ve.Direction, ve.PluginName, ve.Position, ve.Message))
	}
}
