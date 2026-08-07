package format

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/cespare/xxhash/v2"
)

// The specification and the implementation are two descriptions of one format,
// and every round of review has turned up places where they drifted apart: a
// rule stated in one and not the other, a comparison that could never be true,
// a paragraph left behind when the rule it described was replaced. Nothing
// checked one against the other.
//
// These tests do. Every normative sentence in format.md is extracted and
// pinned, so changing or deleting one is a visible event rather than a silent
// edit, and every extracted rule must be claimed by an entry in the invariant
// table (invariants.go) that names the test enforcing it. A rule with no test,
// or a test naming a rule that no longer exists, fails here.
//
// Scope, stated plainly so the coverage is not mistaken for more than it is:
// extraction keys on MUST, so a rule has to be phrased that way to be pinned.
// What is deliberately left out is sub-clauses of a sentence that is already
// pinned (the numbered steps under "writers MUST sort the palette by:" are the
// parent rule's body), rationale explaining why a rule exists, and the
// implementation guidance in the last section, which is advice rather than a
// property of the bytes. Anything that constrains what a conforming file may
// contain belongs in a MUST sentence, and therefore in the table.

const specPath = "format.md"
const specRulesPath = goldenDir + "/spec_rules.txt"

// specRule is one normative sentence with a stable id derived from its text.
type specRule struct {
	id   string
	text string
}

var (
	fenceRe  = regexp.MustCompile("(?s)```.*?```")
	spaceRe  = regexp.MustCompile(`\s+`)
	mustRe   = regexp.MustCompile(`\bMUST\b`)
	sentence = regexp.MustCompile(`(?s)(.+?[.:])(\s|$)`)
)

// extractSpecRules pulls every normative sentence out of the specification.
// Units are table rows and paragraphs, so a sentence wrapped across source
// lines is still one sentence and a table row is not glued to its neighbour.
func extractSpecRules(t *testing.T) []specRule {
	t.Helper()
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatal(err)
	}
	body := fenceRe.ReplaceAllString(string(raw), "\n")

	var units []string
	var para []string
	flushPara := func() {
		if len(para) > 0 {
			units = append(units, strings.Join(para, " "))
			para = nil
		}
	}
	for line := range strings.SplitSeq(body, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			flushPara()
		case strings.HasPrefix(trimmed, "|"):
			flushPara()
			units = append(units, trimmed)
		default:
			para = append(para, trimmed)
		}
	}
	flushPara()

	var rules []specRule
	seen := map[string]bool{}
	for _, u := range units {
		// Table cells rarely end in punctuation, so each cell is its own unit
		// and its trailing fragment counts as a sentence. Without that, a rule
		// stated only in a table goes unnoticed.
		for _, part := range splitUnit(u) {
			end := 0
			for _, m := range sentence.FindAllStringSubmatchIndex(part+" ", -1) {
				add(&rules, seen, part[m[2]:m[3]])
				end = m[3]
			}
			if rest := strings.TrimSpace(part[min(end, len(part)):]); rest != "" {
				add(&rules, seen, rest)
			}
		}
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].id < rules[j].id })
	return rules
}

// splitUnit breaks a table row into cells and leaves prose whole.
func splitUnit(u string) []string {
	if !strings.HasPrefix(u, "|") {
		return []string{u}
	}
	var out []string
	for _, cell := range strings.Split(strings.Trim(u, "|"), "|") {
		if c := strings.TrimSpace(cell); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// add records a normative sentence, skipping anything that is not one.
func add(rules *[]specRule, seen map[string]bool, raw string) {
	s := spaceRe.ReplaceAllString(strings.TrimSpace(raw), " ")
	if !mustRe.MatchString(s) || seen[s] {
		return
	}
	seen[s] = true
	*rules = append(*rules, specRule{id: ruleID(s), text: s})
}

// ruleID is a short stable handle for a normative sentence. It changes when
// the sentence changes, which is the point: an edited rule has to be looked at
// again rather than silently inheriting its predecessor's test.
func ruleID(s string) string {
	return fmt.Sprintf("%08x", uint32(xxhash.Sum64String(s)))
}

// TestSpecRulesPinned fails when a normative sentence is added, changed or
// removed without the change being blessed. Re-run with -update after
// deciding what test covers the new rule.
func TestSpecRulesPinned(t *testing.T) {
	rules := extractSpecRules(t)
	var sb strings.Builder
	for _, r := range rules {
		fmt.Fprintf(&sb, "%s %s\n", r.id, r.text)
	}
	if *update {
		if err := os.WriteFile(specRulesPath, []byte(sb.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s rewritten: %d normative rules", specRulesPath, len(rules))
		return
	}
	want, err := os.ReadFile(specRulesPath)
	if err != nil {
		t.Fatalf("%v (regenerate with -update)", err)
	}
	if sb.String() != string(want) {
		wantIDs := map[string]string{}
		for line := range strings.SplitSeq(strings.TrimSpace(string(want)), "\n") {
			id, text, _ := strings.Cut(line, " ")
			wantIDs[id] = text
		}
		var added, removed []string
		gotIDs := map[string]bool{}
		for _, r := range rules {
			gotIDs[r.id] = true
			if _, ok := wantIDs[r.id]; !ok {
				added = append(added, r.id+" "+trunc(r.text))
			}
		}
		for id, text := range wantIDs {
			if !gotIDs[id] {
				removed = append(removed, id+" "+trunc(text))
			}
		}
		sort.Strings(added)
		sort.Strings(removed)
		t.Fatalf("the specification's normative rules changed.\nadded:\n  %s\nremoved:\n  %s\n"+
			"Claim each new rule in invariants.go, then re-run with -update.",
			strings.Join(added, "\n  "), strings.Join(removed, "\n  "))
	}
}

func trunc(s string) string {
	if len(s) > 100 {
		return s[:100] + "..."
	}
	return s
}

// TestEveryRuleIsClaimed checks the invariant table against the specification
// in both directions: no normative rule may go unclaimed, and no table entry
// may claim a rule that no longer exists.
func TestEveryRuleIsClaimed(t *testing.T) {
	rules := extractSpecRules(t)
	claimed := map[string]*Invariant{}
	for i := range invariants {
		inv := &invariants[i]
		for _, id := range inv.Rules {
			if prev, dup := claimed[id]; dup {
				t.Errorf("rule %s claimed by both %q and %q", id, prev.Name, inv.Name)
			}
			claimed[id] = inv
		}
	}
	live := map[string]string{}
	for _, r := range rules {
		live[r.id] = r.text
	}
	for _, r := range rules {
		if _, ok := claimed[r.id]; !ok {
			t.Errorf("normative rule %s has no invariant claiming it:\n  %s", r.id, trunc(r.text))
		}
	}
	for id, inv := range claimed {
		if _, ok := live[id]; !ok {
			t.Errorf("invariant %q claims rule %s, which the specification no longer states", inv.Name, id)
		}
	}
}

// TestEveryInvariantNamesALiveTest checks that the test each invariant points
// at exists. A renamed or deleted test otherwise leaves the invariant looking
// covered while nothing enforces it.
func TestEveryInvariantNamesALiveTest(t *testing.T) {
	names := testNames(t)
	for _, inv := range invariants {
		if len(inv.Tests) == 0 {
			t.Errorf("invariant %q names no test", inv.Name)
			continue
		}
		if inv.Enforce != Decoded && inv.Enforce != WriterOnly {
			t.Errorf("invariant %q does not say whether readers enforce it", inv.Name)
		}
		for _, name := range inv.Tests {
			if !names[name] {
				t.Errorf("invariant %q names test %s, which does not exist", inv.Name, name)
			}
		}
	}
}

// testNames collects every Test and Fuzz function declared in this package.
func testNames(t *testing.T) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	// Fuzz targets count: a property enforced by fuzzing is enforced.
	decl := regexp.MustCompile(`(?m)^func ((?:Test|Fuzz)\w+)\(`)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range decl.FindAllStringSubmatch(string(b), -1) {
			names[m[1]] = true
		}
	}
	return names
}

// TestNoNormativeTextInFences keeps normative rules where the extractor can
// see them.
//
// extractSpecRules strips fenced blocks before looking for MUST, because a
// fence holds a layout diagram rather than prose. The consequence is that a
// rule written inside one is pinned by nothing, claimed by nothing, and
// invisible to TestEveryRuleIsClaimed — it looks enforced because it is
// written down, and the harness that exists to catch exactly that cannot see
// it.
//
// That is not hypothetical. §3.1's "the override indices strictly ascend"
// lived in a fence, so nothing required a test for it, and the index chain
// turned out to accept a delta that wrapped onto a lower index — a second
// encoding of one palette, in a format whose whole doctrine is that there is
// exactly one. It was found by a hostile-input pass days before the freeze,
// and closing it after would have needed a version bump. Three more rules
// were fence-only when this test was written: the light entry's reserved
// bits, an override's version constraints, and the structure origin's range.
// All three were enforced in code and pinned by nothing.
//
// So: state the rule in prose, and let the fence describe the bytes.
func TestNoNormativeTextInFences(t *testing.T) {
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, fence := range fenceRe.FindAllString(string(raw), -1) {
		for line := range strings.SplitSeq(fence, "\n") {
			if mustRe.MatchString(line) {
				t.Errorf("normative text inside a fenced block, where no rule is pinned:\n\t%s\n"+
					"State it in prose after the fence and describe the bytes here instead.",
					strings.TrimSpace(line))
			}
		}
	}
}
