package model

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDeriveTitle(t *testing.T) {
	tests := []struct {
		name      string
		explicit  string
		objective string
		feature   string
		want      string
	}{
		{name: "explicit override", explicit: "  Keep: explicit punctuation!  ", objective: "ignored objective", feature: "ignored-feature", want: "Keep: explicit punctuation!"},
		{name: "punctuation", objective: "Fix retries, preserve errors, and report partial success.", feature: "retry", want: "Fix retries, preserve errors, and report partial success."},
		{name: "multiline whitespace", objective: "\n  Simplify\tordered work\n\nacross   drivers  \n", feature: "ordered-work", want: "Simplify ordered work across drivers"},
		{name: "feature fallback", explicit: " \t ", objective: "\n\t", feature: "cross_driver.discovery", want: "Cross driver discovery"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := DeriveTitle(test.explicit, test.objective, test.feature); got != test.want {
				t.Fatalf("DeriveTitle() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestDeriveTitleBoundsUnicodeWithoutSplittingUTF8(t *testing.T) {
	objective := strings.Repeat("界", MaxTitleRunes+20)
	title := DeriveTitle("", objective, "fallback")
	if !utf8.ValidString(title) {
		t.Fatalf("derived title is invalid UTF-8: %q", title)
	}
	if got := utf8.RuneCountInString(title); got != MaxTitleRunes {
		t.Fatalf("derived title rune count = %d, want %d", got, MaxTitleRunes)
	}
	if !strings.HasSuffix(title, "…") {
		t.Fatalf("derived title = %q, want ellipsis", title)
	}
}
