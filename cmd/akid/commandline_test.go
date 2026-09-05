package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestSplitCommandLine(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{`uv run bot.py`, []string{"uv", "run", "bot.py"}},
		{`python "two words.py" --name 'chino bot'`, []string{"python", "two words.py", "--name", "chino bot"}},
		{`echo \"$HOME\"`, []string{"echo", `"$HOME"`}},
		{`"" ''`, []string{"", ""}},
	}
	for _, test := range tests {
		got, err := splitCommandLine(test.input)
		if err != nil || !reflect.DeepEqual(got, test.want) {
			t.Fatalf("split %q = %#v, %v; want %#v", test.input, got, err, test.want)
		}
	}
	for _, input := range []string{``, `echo "unterminated`, `echo trailing\`} {
		if _, err := splitCommandLine(input); err == nil {
			t.Fatalf("accepted malformed command %q", input)
		}
	}
}

func TestExpandStartCommand(t *testing.T) {
	got, err := expandStartCommand([]string{`uv run bot.py`, `--port`, `8080`})
	if err != nil || !reflect.DeepEqual(got, []string{"uv", "run", "bot.py", "--port", "8080"}) {
		t.Fatalf("expanded command = %#v, %v", got, err)
	}
	if _, err := expandStartCommand(nil); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatal("missing command was accepted")
	}
}

func TestResolveExecutableKeepsExplicitAndMissingCommands(t *testing.T) {
	if got := resolveExecutable("/opt/bin/uv"); got != "/opt/bin/uv" {
		t.Fatal(got)
	}
	if got := resolveExecutable("akid-command-that-does-not-exist"); got != "akid-command-that-does-not-exist" {
		t.Fatal(got)
	}
	if got := resolveExecutable("sh"); !strings.HasSuffix(got, "/sh") {
		t.Fatalf("sh was not resolved: %q", got)
	}
}
