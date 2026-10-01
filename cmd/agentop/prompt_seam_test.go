package main

import (
	"bytes"
	"io"
	"reflect"
	"strings"
	"testing"
)

// answerPrompt makes *prompt answer from a canned line for one test. It goes through
// the real confirmFrom, so the prompt text lands in the output exactly as a person at
// a terminal would see it — the characterization tests depend on that.
func answerPrompt(t *testing.T, prompt *func(io.Writer) bool, line string) {
	t.Helper()
	saved := *prompt
	*prompt = func(w io.Writer) bool { return confirmFrom(strings.NewReader(line), w) }
	t.Cleanup(func() { *prompt = saved })
}

// The seams must default to the real terminal prompt: a var left pointing at a stub
// would make every interactive install answer itself.
func TestPromptSeams_DefaultToTheTerminalPrompt(t *testing.T) {
	want := reflect.ValueOf(confirm).Pointer()
	for name, got := range map[string]func(io.Writer) bool{
		"serviceConfirm":    serviceConfirm,
		"claudeCodeConfirm": claudeCodeConfirm,
	} {
		if reflect.ValueOf(got).Pointer() != want {
			t.Errorf("%s does not default to confirm", name)
		}
	}
}

func TestAnswerPrompt_UsesTheRealPromptText(t *testing.T) {
	answerPrompt(t, &serviceConfirm, "y\n")
	var out bytes.Buffer
	if !serviceConfirm(&out) {
		t.Error("a canned yes was read as no")
	}
	if out.String() != "Apply? [y/N] " {
		t.Errorf("prompt text = %q", out.String())
	}
}
