package scaffold

import (
	"bufio"
	"bytes"
	"errors"
	"strings"
	"testing"
)

func pickWith(t *testing.T, input string, labels, values []string) (string, string, error) {
	t.Helper()
	out := &bytes.Buffer{}
	got, err := Pick(FormOptions{In: bufio.NewReader(strings.NewReader(input)), Out: out, Accessible: true}, "Which one?", "Pass --one to skip.", labels, values)
	return got, out.String(), err
}

func TestPickReturnsTheValueOfTheChosenLabel(t *testing.T) {
	labels, values := []string{"alpha  first one", "beta  second one", "gamma  third"}, []string{"a", "b", "g"}
	for input, want := range map[string]string{"1\n": "a", "2\n": "b", "3\n": "g", "\n": "a", "9\n0\nx\n3\n": "g"} {
		got, out, err := pickWith(t, input, labels, values)
		if err != nil || got != want {
			t.Errorf("input %q: got %q, %v; want %q", input, got, err, want)
		}
		for _, l := range labels {
			if !strings.Contains(out, l) {
				t.Errorf("the prompt must show %q:\n%s", l, out)
			}
		}
		if !strings.Contains(out, "Which one?") || !strings.Contains(out, "Pass --one to skip.") {
			t.Errorf("the prompt shows its title and description:\n%s", out)
		}
	}
}

func TestPickWhenTheInputEndsIsAnAbort(t *testing.T) {
	if _, _, err := pickWith(t, "", []string{"a", "b"}, []string{"a", "b"}); !errors.Is(err, ErrAborted) {
		t.Errorf("an input that ends is ErrAborted, got %v", err)
	}
}

func TestPickRefusesNothingToChooseFrom(t *testing.T) {
	for name, c := range map[string]struct{ labels, values []string }{
		"empty":    {nil, nil},
		"mismatch": {[]string{"a", "b"}, []string{"a"}},
	} {
		if _, _, err := pickWith(t, "1\n", c.labels, c.values); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
