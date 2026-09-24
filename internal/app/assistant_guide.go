package app

import (
	"fmt"
	"io/fs"
	"math"
	"sort"
	"strings"
	"sync"

	"attesttag/guide"
)

// Searching the product's own documentation.
//
// The console assistant is asked two different kinds of question, and only one of them is about
// the organisation's data. "Which channels can reach GitHub" is a read. "How do I set things up
// so somebody can approve a repository invite" is not in any table — it is in the guide, and a
// model asked it without the guide will compose an answer out of how such products usually work.
// That answer is confident, specific, and wrong in the details that matter: which permission the
// token needs, that path prefixes and methods are mandatory on a grant connection, that matching
// is a plain prefix with no wildcard. Somebody then goes and tries it.
//
// Keyword search rather than embeddings, deliberately. The corpus is fifteen files that change
// when the maintainer edits them, the vocabulary is the product's own — "bundle", "scope",
// "grant", "prefix" — and questions use those words because the console does. Embeddings would
// add a model call, an index to rebuild on deploy, and a failure mode where the guide silently
// stops being searchable; scoring words against headings gets the right section for the price of
// walking 164KB in memory.

// guideSection is one heading's worth of the guide.
type guideSection struct {
	file, title, body string
	// words is the section's vocabulary, counted once at startup, and length its total. Sections
	// are scored against a query many times over the life of the process and never change.
	words  map[string]int
	length int
}

var (
	guideOnce     sync.Once
	guideSections []guideSection
	// guideDF is how many sections each word appears in, and guideAvgLen the mean section
	// length. Both are what turns counting words into ranking them; see bm25.
	guideDF     map[string]int
	guideAvgLen = 1.0
)

// guideIDF is worth most for a word that appears in one section and almost nothing for one in
// every section.
func guideIDF(term string) float64 {
	n := float64(guideDF[term])
	N := float64(len(guideSections))
	if N == 0 {
		return 0
	}
	return math.Log(1 + (N-n+0.5)/(n+0.5))
}

// guideWords splits text the way both sides of the search have to agree on: lower case, letters
// and digits, three characters or more. Short words are dropped because "a", "of" and "to" match
// every section and rank none of them.
func guideWords(s string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		if len(w) >= 3 {
			out = append(out, w)
		}
	}
	return out
}

// loadGuide splits every page on its headings. A section is the unit worth returning: a whole
// page is too much to put in front of a model that asked one question, and a paragraph on its
// own has lost the heading that says what it is about.
func loadGuide() []guideSection {
	guideOnce.Do(func() {
		entries, err := fs.ReadDir(guide.Files, ".")
		if err != nil {
			return
		}
		for _, e := range entries {
			raw, err := fs.ReadFile(guide.Files, e.Name())
			if err != nil {
				continue
			}
			title, body := "", strings.Builder{}
			flush := func() {
				text := strings.TrimSpace(body.String())
				if text == "" {
					return
				}
				sec := guideSection{file: e.Name(), title: title, body: text, words: map[string]int{}}
				// The heading counts three times: a section called "Access bundles" is what
				// somebody asking about access bundles wants, even when a longer section
				// mentions the words more often in passing.
				for _, w := range guideWords(title) {
					sec.words[w] += 3
				}
				for _, w := range guideWords(text) {
					sec.words[w]++
				}
				for _, n := range sec.words {
					sec.length += n
				}
				guideSections = append(guideSections, sec)
			}
			// A "#" inside a fenced block is a shell comment, not a heading. The guide is mostly
			// runbook, so most of its "#" lines are comments above a command — and splitting on
			// them cut plans.md into fragments titled things like "Move one to pro with a $50
			// monthly budget", which is a sentence about one command rather than a section
			// anybody could be looking for.
			fenced := false
			for _, line := range strings.Split(string(raw), "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "```") {
					fenced = !fenced
				}
				if !fenced && strings.HasPrefix(line, "#") {
					flush()
					title, body = strings.TrimSpace(strings.TrimLeft(line, "# ")), strings.Builder{}
					continue
				}
				body.WriteString(line)
				body.WriteString("\n")
			}
			flush()
		}
		// Document frequency and mean length, once every section is in.
		guideDF = map[string]int{}
		total := 0
		for _, s := range guideSections {
			total += s.length
			for w := range s.words {
				guideDF[w]++
			}
		}
		if len(guideSections) > 0 {
			guideAvgLen = float64(total) / float64(len(guideSections))
		}
	})
	return guideSections
}

const (
	guideHits     = 4    // sections returned
	guideSectionC = 2400 // characters of one section
	// BM25's usual constants: k1 saturates repetition, b decides how much a long section is
	// penalised for it.
	bm25K1 = 1.2
	bm25B  = 0.75
)

// Words that are in every question and no help in finding the answer. Kept short on purpose:
// this is a list of question-asking words, not of English — "access", "grant" and "cost" are
// exactly the words that should rank, and a borrowed stopword list would be full of near misses.
var guideStop = map[string]bool{
	"how": true, "the": true, "what": true, "does": true, "can": true, "you": true, "for": true,
	"and": true, "with": true, "this": true, "that": true, "are": true, "not": true, "but": true,
	"from": true, "when": true, "where": true, "who": true, "why": true, "would": true, "should": true,
	"about": true, "into": true, "out": true, "get": true, "set": true, "use": true, "using": true,
	"have": true, "has": true, "was": true, "were": true, "will": true, "there": true, "then": true,
	"than": true, "its": true, "our": true, "your": true, "their": true, "they": true, "some": true,
	"any": true, "one": true, "all": true, "every": true, "each": true, "more": true, "most": true,
}

func queryTerms(query string) []string {
	var out []string
	for _, w := range guideWords(query) {
		if !guideStop[w] {
			out = append(out, w)
		}
	}
	return out
}

// bm25 scores one section against a question.
//
// The first attempt at this counted matching words, and it ranked the guide's longest section —
// a table of every file in the repository — top for "how do I approve a github repository
// invite", because a long enough section mentions everything. BM25 is the standard answer to
// exactly that: term frequency saturates, length is divided out, and a word that appears in most
// sections is worth almost nothing while a rare one carries the match. "repository" is in half
// the guide; "bundle" and "prefix" are not, and they are what the question was really about.
func bm25(s guideSection, terms []string) float64 {
	score := 0.0
	norm := bm25K1 * (1 - bm25B + bm25B*float64(s.length)/guideAvgLen)
	for _, t := range terms {
		f := float64(s.words[t])
		if f == 0 {
			// Only then a prefix match, and only a real one: "bundles" against "bundle",
			// "granting" against "grant". Four characters minimum, because three-letter
			// prefixes match half the dictionary, and at a discount because it is a guess
			// about English rather than something the writer typed.
			for w, n := range s.words {
				if len(t) >= 4 && len(w) >= 4 && (strings.HasPrefix(w, t) || strings.HasPrefix(t, w)) {
					f += float64(n) * 0.5
				}
			}
		}
		if f == 0 {
			continue
		}
		score += guideIDF(t) * (f * (bm25K1 + 1)) / (f + norm)
	}
	// A section whose heading is what was asked about wins over one that happens to say the
	// words. "Access bundles" beats the data-model table that lists every column in the
	// database, which mentions bundles, budgets and costs because it mentions everything.
	if score > 0 && len(terms) > 0 {
		inTitle := 0
		for _, t := range terms {
			for _, w := range guideWords(s.title) {
				if w == t || (len(t) >= 4 && len(w) >= 4 && (strings.HasPrefix(w, t) || strings.HasPrefix(t, w))) {
					inTitle++
					break
				}
			}
		}
		score *= 1 + 2*float64(inTitle)/float64(len(terms))
	}
	return score
}

// searchGuide returns the sections that best answer a question, longest-match first. It never
// returns nothing when the guide has anything at all: a question that scores zero gets the
// contents instead, because "here is what the documentation covers" is a better answer than
// silence, and silence is what makes a model fall back on inventing the configuration.
func searchGuide(query string) string {
	secs := loadGuide()
	if len(secs) == 0 {
		return "the guide is not available in this build"
	}
	terms := queryTerms(query)
	type hit struct {
		i     int
		score float64
	}
	var hits []hit
	for i, s := range secs {
		if score := bm25(s, terms); score > 0 {
			hits = append(hits, hit{i, score})
		}
	}
	if len(hits) == 0 {
		var b strings.Builder
		b.WriteString("Nothing in the guide matched that. The pages are:\n")
		seen := map[string]bool{}
		for _, s := range secs {
			if !seen[s.file] {
				seen[s.file] = true
				fmt.Fprintf(&b, "- %s\n", s.file)
			}
		}
		return b.String()
	}
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].score > hits[b].score })
	if len(hits) > guideHits {
		hits = hits[:guideHits]
	}
	var b strings.Builder
	for _, h := range hits {
		s := secs[h.i]
		body, cut := cutRunes(s.body, guideSectionC)
		fmt.Fprintf(&b, "\n## %s — %s\n%s\n", s.file, s.title, body)
		if cut {
			b.WriteString("[…section continues]\n")
		}
	}
	return strings.TrimSpace(b.String())
}
