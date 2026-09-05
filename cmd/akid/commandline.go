package main

import (
	"fmt"
	"os/exec"
	"strings"
)

// splitCommandLine handles the small shell-like subset useful for `akid start`:
// whitespace separates arguments, single/double quotes preserve whitespace, and
// backslash escapes the next character. It deliberately does not expand shell
// variables, glob patterns, command substitutions, or redirections.
func splitCommandLine(input string) ([]string, error) {
	var result []string
	var current strings.Builder
	var quote rune
	escaped, started := false, false
	flush := func() {
		if started {
			result = append(result, current.String())
			current.Reset()
			started = false
		}
	}
	for _, r := range input {
		if escaped {
			current.WriteRune(r)
			escaped, started = false, true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
				started = true
			} else if r == '\\' && quote == '"' {
				escaped, started = true, true
			} else {
				current.WriteRune(r)
				started = true
			}
			continue
		}
		switch r {
		case '\'', '"':
			quote, started = r, true
		case '\\':
			escaped, started = true, true
		case ' ', '\t', '\n', '\r':
			flush()
		default:
			current.WriteRune(r)
			started = true
		}
	}
	if escaped {
		return nil, fmt.Errorf("command ends with an escape")
	}
	if quote != 0 {
		return nil, fmt.Errorf("command has an unterminated quote")
	}
	flush()
	if len(result) == 0 {
		return nil, fmt.Errorf("command is empty")
	}
	return result, nil
}

func expandStartCommand(args []string) ([]string, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("command is required")
	}
	first, err := splitCommandLine(args[0])
	if err != nil {
		return nil, err
	}
	return append(first, args[1:]...), nil
}

// resolveExecutable makes a bare command independent of the daemon's login
// environment. This matters for tools installed in ~/.local/bin (for example
// uv), while preserving unresolved commands so the daemon can return its normal
// SPAWN_FAILED result and diagnostics.
func resolveExecutable(command string) string {
	if strings.ContainsRune(command, '/') {
		return command
	}
	if resolved, err := exec.LookPath(command); err == nil {
		return resolved
	}
	return command
}
