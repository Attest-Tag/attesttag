package app

import (
	"errors"
	"strings"
)

// SplitCommand turns a test command line into argv without a shell. The worker used to hand the
// console's test_cmd to sh -c, which made the setting a shell in the worker container for anyone
// who could edit a repository connection. Now it is a program and its arguments: whitespace
// separates them, single or double quotes group them, and anything a shell would interpret —
// redirection, pipes, substitution, chaining — is refused rather than quietly stripped.
func SplitCommand(line string) ([]string, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, nil
	}
	if strings.ContainsAny(line, "|&;<>`$(){}\n\r\\") {
		return nil, errors.New("test_cmd must be a plain program and arguments: no pipes, redirection, substitution or chaining (run a script from the repository instead)")
	}
	var argv []string
	var cur strings.Builder
	quote := rune(0)
	in := false
	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, in = r, true
		case r == ' ' || r == '\t':
			if in {
				argv = append(argv, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if quote != 0 {
		return nil, errors.New("test_cmd has an unclosed quote")
	}
	if in {
		argv = append(argv, cur.String())
	}
	return argv, nil
}
