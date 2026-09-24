package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"attesttag/internal/app"
)

// GitHub is the REST client for the one write the worker makes there: opening the pull request.
type GitHub struct {
	base   string
	token  string
	client *http.Client
}

var githubAPI = "https://api.github.com"

func newGitHub(token string) *GitHub {
	return &GitHub{base: githubAPI, token: token, client: &http.Client{Timeout: 30 * time.Second}}
}

// newGitHubFor is the same client, at whatever base the options name — github.com everywhere
// except a local run pointed at a stub.
func newGitHubFor(token string, o Options) *GitHub {
	g := newGitHub(token)
	if o.Mode == "local" && o.GitHubAPI != "" {
		g.base = o.GitHubAPI
	}
	return g
}

func (g *GitHub) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "attesttag-worker")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, nil
}

type prInput struct {
	Title, Head, Base, Body string
	Draft                   bool
}

type prResponse struct {
	HTMLURL string `json:"html_url"`
	Number  int    `json:"number"`
	Draft   bool   `json:"draft"`
	Head    struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (p prResponse) job() *app.JobPR {
	return &app.JobPR{URL: p.HTMLURL, Number: p.Number, Branch: p.Head.Ref, Base: p.Base.Ref, HeadSHA: p.Head.SHA, Draft: p.Draft}
}

func githubMessage(raw []byte) string {
	var e struct {
		Message string `json:"message"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	json.Unmarshal(raw, &e)
	msg := e.Message
	for _, x := range e.Errors {
		if x.Message != "" {
			msg += "; " + x.Message
		}
	}
	return strings.TrimPrefix(msg, "; ")
}

// createPR opens the pull request, reusing an open one for the same branch when GitHub says it
// already exists. Failures come back as pr_create_failed with what to do by hand.
func (g *GitHub) createPR(ctx context.Context, repo string, in prInput) (*app.JobPR, error) {
	code, raw, err := g.do(ctx, "POST", "/repos/"+repo+"/pulls", map[string]any{
		"title": in.Title, "head": in.Head, "base": in.Base, "body": in.Body, "draft": in.Draft, "maintainer_can_modify": true})
	if err != nil {
		return nil, stepErr("pr_create_failed", "GitHub unreachable: "+err.Error())
	}
	byHand := fmt.Sprintf(" The branch is pushed; open it by hand: gh pr create --repo %s --base %s --head %s --draft", repo, in.Base, in.Head)
	switch code {
	case 201:
		var pr prResponse
		if err := json.Unmarshal(raw, &pr); err != nil || pr.HTMLURL == "" {
			return nil, stepErr("pr_create_failed", "GitHub returned an unreadable pull request."+byHand)
		}
		if pr.Base.Ref != in.Base {
			return pr.job(), stepErr("pr_base_mismatch", fmt.Sprintf("pull request %s targets %s, not %s", pr.HTMLURL, pr.Base.Ref, in.Base))
		}
		return pr.job(), nil
	case 422:
		msg := githubMessage(raw)
		if strings.Contains(strings.ToLower(msg), "already exists") {
			if pr, err := g.findOpenPR(ctx, repo, in.Head); err == nil && pr != nil {
				return pr, nil
			}
		}
		if strings.Contains(strings.ToLower(msg), "no commits between") {
			return nil, stepErr("empty_diff", "GitHub sees no commits between the branches")
		}
		return nil, stepErr("pr_create_failed", "GitHub refused the pull request: "+msg+byHand)
	case 403, 404:
		return nil, stepErr("pr_create_failed", fmt.Sprintf("the token cannot open pull requests on %s (HTTP %d: %s); it needs Pull requests: write.%s", repo, code, githubMessage(raw), byHand))
	}
	return nil, stepErr("pr_create_failed", fmt.Sprintf("GitHub answered %d: %s.%s", code, cut(githubMessage(raw), 300), byHand))
}

func (g *GitHub) findOpenPR(ctx context.Context, repo, branch string) (*app.JobPR, error) {
	owner := repo
	if i := strings.Index(repo, "/"); i > 0 {
		owner = repo[:i]
	}
	code, raw, err := g.do(ctx, "GET", "/repos/"+repo+"/pulls?state=open&head="+url.QueryEscape(owner+":"+branch), nil)
	if err != nil || code != 200 {
		return nil, fmt.Errorf("list pull requests: %d %v", code, err)
	}
	var prs []prResponse
	if err := json.Unmarshal(raw, &prs); err != nil || len(prs) == 0 {
		return nil, fmt.Errorf("no open pull request for %s", branch)
	}
	return prs[0].job(), nil
}
