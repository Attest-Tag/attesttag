package app

import (
	"fmt"
	"strings"
)

// A recipe an admin typed into the console is checked here, before it is stored, rather than in
// the worker where the only way to report a mistake is a failed job. Same rule as the older
// single test command: a step is a program and its arguments, never a shell line, so the person
// who can edit a repository connection is not thereby given a shell in the worker container.

const (
	recipeMaxSteps    = 8
	recipeMaxArgs     = 40
	recipeMaxArgBytes = 256
)

func validateRecipeInput(in *Recipe) (*Recipe, error) {
	if in == nil {
		return nil, nil
	}
	out := &Recipe{Source: RecipeSourceConnection, Ecosystem: strings.TrimSpace(in.Ecosystem), Workdir: cleanWorkdir(in.Workdir)}
	if len(in.Setup) > recipeMaxSteps {
		return nil, fmt.Errorf("at most %d install steps", recipeMaxSteps)
	}
	for i := range in.Setup {
		s, err := validateStepInput(&in.Setup[i], fmt.Sprintf("install step %d", i+1))
		if err != nil {
			return nil, err
		}
		out.Setup = append(out.Setup, *s)
	}
	var err error
	if out.Build, err = validateStepInput(in.Build, "build"); err != nil {
		return nil, err
	}
	if out.Lint, err = validateStepInput(in.Lint, "lint"); err != nil {
		return nil, err
	}
	if out.Test, err = validateStepInput(in.Test, "test"); err != nil {
		return nil, err
	}
	for k, v := range in.Tools {
		k, v = strings.TrimSpace(strings.ToLower(k)), strings.TrimSpace(v)
		if k == "" || v == "" {
			continue
		}
		if len(k) > 32 || len(v) > 32 {
			return nil, fmt.Errorf("toolchain names and versions are at most 32 characters")
		}
		if out.Tools == nil {
			out.Tools = map[string]string{}
		}
		out.Tools[k] = v
	}
	for _, sv := range in.Services {
		if sv = strings.TrimSpace(sv); sv != "" {
			out.Services = append(out.Services, sv)
		}
	}
	if len(out.Services) > 6 {
		return nil, fmt.Errorf("at most 6 services")
	}
	return out, nil
}

func validateStepInput(in *RecipeStep, label string) (*RecipeStep, error) {
	if in == nil {
		return nil, nil
	}
	argv := in.Argv
	// The console sends one line in `run`; the API also takes an argv array for anyone driving
	// it directly. Both end up as argv, checked the same way.
	if len(argv) == 0 {
		line := strings.TrimSpace(in.Run)
		if line == "" {
			return nil, nil
		}
		var err error
		if argv, err = SplitCommand(line); err != nil {
			return nil, fmt.Errorf("%s: %w", label, err)
		}
	}
	if len(argv) == 0 {
		return nil, nil
	}
	if len(argv) > recipeMaxArgs {
		return nil, fmt.Errorf("%s: at most %d arguments", label, recipeMaxArgs)
	}
	for _, a := range argv {
		if len(a) > recipeMaxArgBytes {
			return nil, fmt.Errorf("%s: an argument is over %d characters", label, recipeMaxArgBytes)
		}
		if strings.ContainsAny(a, "\n\r\x00") {
			return nil, fmt.Errorf("%s: a command cannot contain a newline", label)
		}
	}
	if strings.ContainsAny(argv[0], "|&;<>`$(){}") {
		return nil, fmt.Errorf("%s must be a program and its arguments, not a shell line (run a script from the repository instead)", label)
	}
	if in.TimeoutS < 0 || in.TimeoutS > 3600 {
		return nil, fmt.Errorf("%s: timeout_s must be between 0 and 3600", label)
	}
	return &RecipeStep{Name: label, Argv: argv, Dir: cleanWorkdir(in.Dir), TimeoutS: in.TimeoutS, Optional: in.Optional}, nil
}

// cleanWorkdir keeps a directory to a relative path inside the repository.
func cleanWorkdir(d string) string {
	d = strings.TrimSpace(strings.ReplaceAll(d, "\\", "/"))
	d = strings.TrimPrefix(d, "./")
	if d == "" || d == "." || strings.HasPrefix(d, "/") || strings.Contains(d, "..") || len(d) > 200 {
		return ""
	}
	return strings.TrimSuffix(d, "/")
}
