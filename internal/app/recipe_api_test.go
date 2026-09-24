package app

import (
	"context"
	"strings"
	"testing"
)

// The recipe an admin sets in the console has to survive the whole way: validated on save,
// stored on the connection, snapshotted into the spec at dispatch, and handed to the worker on
// the claim. A break anywhere in that chain looks like the worker ignoring the setting.
func TestConnectionRecipeReachesTheWorker(t *testing.T) {
	h := newJobHarness(t, nil)
	ctx := context.Background()

	conn, err := h.b.store.Connection(ctx, orgID, h.connID)
	if err != nil || conn == nil {
		t.Fatal(err)
	}
	in := &connectionInput{BundleID: conn.BundleID, Name: conn.Name, Preset: "github", CredType: "bearer",
		Recipe: &Recipe{Workdir: "services/api", Test: &RecipeStep{Run: "go test -race ./..."},
			Build: &RecipeStep{Run: "go build ./..."}, Tools: map[string]string{"Go": " 1.25 "}}}
	built, _, err := h.b.buildConnection(in, conn)
	if err != nil {
		t.Fatalf("saving a recipe: %v", err)
	}
	if built.Recipe == nil || built.Recipe.Source != RecipeSourceConnection ||
		built.Recipe.Test.String() != "go test -race ./..." || built.Recipe.Workdir != "services/api" ||
		built.Recipe.Tools["go"] != "1.25" {
		t.Fatalf("validated recipe: %+v", built.Recipe)
	}
	built.ID, built.Repo, built.Status = h.connID, conn.Repo, "active"
	if err := h.b.store.UpdateConnection(ctx, orgID, built, nil); err != nil {
		t.Fatal(err)
	}

	// It comes back off the row…
	again, err := h.b.store.Connection(ctx, orgID, h.connID)
	if err != nil || again.Recipe == nil || again.Recipe.Test.String() != "go test -race ./..." {
		t.Fatalf("stored recipe: %+v (%v)", again.Recipe, err)
	}
	// …reaches the confirm card…
	spec := testSpec(h.connID)
	spec.Constraints = h.b.jobs.constraints(h.b.settings.Get(ctx, orgID), again, spec.Kind)
	if card := jobConfirmSummary(spec); !strings.Contains(card, "go test -race ./...") || !strings.Contains(card, "set in the console") {
		t.Errorf("the confirm card does not say what will run:\n%s", card)
	}
	// …and is what the worker is handed at dispatch.
	j, err := h.b.jobs.Dispatch(ctx, DispatchRequest{OrgID: orgID, TeamID: "T1", Spec: testSpec(h.connID), ApprovedBy: "U2", Approval: "confirm"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	l := h.fd.launches[0]
	code, body := h.call(t, "POST", "/api/worker/jobs/1/claim", l.Token, JobClaimRequest{Worker: JobWorkerInfo{Mode: "fake"}}, "")
	if code != 200 {
		t.Fatalf("claim: %d %v", code, body)
	}
	job, _ := body["job"].(map[string]any)
	sp, _ := job["spec"].(map[string]any)
	cons, _ := sp["constraints"].(map[string]any)
	rec, _ := cons["recipe"].(map[string]any)
	test, _ := rec["test"].(map[string]any)
	argv, _ := test["argv"].([]any)
	if rec["workdir"] != "services/api" || len(argv) != 4 || argv[0] != "go" || argv[2] != "-race" {
		t.Fatalf("the worker was not handed the recipe: %v", rec)
	}
	_ = j

	// A shell line is refused on save rather than becoming a shell in the worker container.
	bad := &connectionInput{BundleID: conn.BundleID, Name: conn.Name, Preset: "github", CredType: "bearer",
		Recipe: &Recipe{Test: &RecipeStep{Run: "go test ./... && curl http://evil.example"}}}
	if _, _, err := h.b.buildConnection(bad, conn); err == nil {
		t.Error("a shell line was accepted as a recipe command")
	}
	// So is a directory that climbs out of the repository.
	esc := &connectionInput{BundleID: conn.BundleID, Name: conn.Name, Preset: "github", CredType: "bearer",
		Recipe: &Recipe{Workdir: "../../etc", Test: &RecipeStep{Run: "go test ./..."}}}
	out, _, err := h.b.buildConnection(esc, conn)
	if err != nil {
		t.Fatal(err)
	}
	if out.Recipe.Workdir != "" {
		t.Errorf("a workdir escaped the repository: %q", out.Recipe.Workdir)
	}
}
