package main

// Guard for #2310: every place a TEST HARNESS boots a Stillwater server must
// set SW_AI_BLOCKLIST_URL (empty), so no test job downloads the AI-image
// blocklist from GitHub. A review found one boot site (the base-path a11y
// server) that had been missed by hand; this test finds boot sites
// mechanically so the next one cannot be missed the same way.
//
// What counts as a boot site: an assignment of SW_DB_PATH (a harness server
// always sets it, because the in-code default /config/stillwater.db is not
// writable on a developer machine or CI runner) in the Makefile, a workflow,
// a script, or a JS test helper. The assignment's block must also assign
// SW_AI_BLOCKLIST_URL. A block is the command the assignment belongs to:
// backslash-continued lines for Makefile and shell, and the run of lines at
// the same or deeper indentation for YAML env maps and JS object literals.
// Operator and developer paths (the Makefile run and uat targets) are
// exempt: fetching the list is correct there.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	dbPathAssign    = regexp.MustCompile(`SW_DB_PATH\s*[:=]`)
	blocklistAssign = regexp.MustCompile(`SW_AI_BLOCKLIST_URL\s*[:=]`)
	makeTarget      = regexp.MustCompile(`^([A-Za-z0-9_.-]+)\s*:`)
)

// exemptMakeTargets are operator/dev paths that should fetch the real list.
var exemptMakeTargets = map[string]bool{"run": true, "uat": true}

// harnessBootSite is one SW_DB_PATH assignment and the block around it.
type harnessBootSite struct {
	file  string
	line  int
	block string
}

func isCommentLine(s string) bool {
	t := strings.TrimSpace(s)
	return strings.HasPrefix(t, "#") || strings.HasPrefix(t, "//") || strings.HasPrefix(t, "*")
}

func indentOf(s string) int { return len(s) - len(strings.TrimLeft(s, " \t")) }

// continuationBlock returns the backslash-continued command containing i.
func continuationBlock(lines []string, i int) (int, int) {
	lo, hi := i, i
	for lo > 0 && strings.HasSuffix(strings.TrimRight(lines[lo-1], " "), `\`) {
		lo--
	}
	for hi < len(lines)-1 && strings.HasSuffix(strings.TrimRight(lines[hi], " "), `\`) {
		hi++
	}
	return lo, hi
}

// indentBlock returns the contiguous non-blank lines at i's indentation or deeper.
func indentBlock(lines []string, i int) (int, int) {
	ind := indentOf(lines[i])
	in := func(j int) bool { return strings.TrimSpace(lines[j]) != "" && indentOf(lines[j]) >= ind }
	lo, hi := i, i
	for lo > 0 && in(lo-1) {
		lo--
	}
	for hi < len(lines)-1 && in(hi+1) {
		hi++
	}
	return lo, hi
}

// findHarnessBootSites scans one file's content for SW_DB_PATH assignments.
func findHarnessBootSites(name, content string) []harnessBootSite {
	lines := strings.Split(content, "\n")
	isMake := filepath.Base(name) == "Makefile"
	byIndent := strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".js")
	var sites []harnessBootSite
	target := ""
	for i, l := range lines {
		if isMake {
			if m := makeTarget.FindStringSubmatch(l); m != nil && !strings.HasPrefix(l, "\t") {
				target = m[1]
			}
		}
		if isCommentLine(l) || !dbPathAssign.MatchString(l) {
			continue
		}
		if isMake && exemptMakeTargets[target] {
			continue
		}
		var lo, hi int
		if byIndent {
			lo, hi = indentBlock(lines, i)
		} else {
			lo, hi = continuationBlock(lines, i)
		}
		sites = append(sites, harnessBootSite{file: name, line: i + 1, block: strings.Join(lines[lo:hi+1], "\n")})
	}
	return sites
}

// harnessFiles lists the files a test-harness boot site can live in.
func harnessFiles(t *testing.T, root string) []string {
	t.Helper()
	files := []string{filepath.Join(root, "Makefile")}
	for _, g := range []string{".github/workflows/*.yml", "scripts/*.sh"} {
		m, err := filepath.Glob(filepath.Join(root, g))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	err := filepath.WalkDir(filepath.Join(root, "tests"), func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "node_modules" {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(p, ".js") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestHarnessServersSkipAIBlocklist(t *testing.T) {
	root := filepath.Join("..", "..")
	var sites []harnessBootSite
	for _, f := range harnessFiles(t, root) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(root, f)
		sites = append(sites, findHarnessBootSites(rel, string(b))...)
	}
	// Precondition: the scan must find the known boot sites (test-a11y,
	// bruno-ci, the two workflow jobs, the smoke, the base-path server), or a
	// scanner bug would pass by finding nothing.
	if len(sites) < 6 {
		t.Fatalf("found %d harness boot sites, want at least 6; the scanner is broken: %+v", len(sites), sites)
	}
	for _, s := range sites {
		if !blocklistAssign.MatchString(s.block) {
			t.Errorf("%s:%d boots a test server without SW_AI_BLOCKLIST_URL; set it empty so the harness never downloads the AI blocklist (#2310). Block:\n%s", s.file, s.line, s.block)
		}
	}
}

// The scanner itself, on synthetic input: a site missing the variable is
// reported, a site carrying it passes, and the exempt dev target is skipped.
func TestFindHarnessBootSites(t *testing.T) {
	js := "spawn(BIN, [], {\n  env: {\n    ...process.env,\n    SW_DB_PATH: db,\n    SW_PORT: p,\n  },\n});\n"
	sites := findHarnessBootSites("tests/x.js", js)
	if len(sites) != 1 || blocklistAssign.MatchString(sites[0].block) {
		t.Fatalf("JS site without the var: got %+v, want one site missing it", sites)
	}
	jsOK := strings.Replace(js, "SW_PORT: p,", "SW_PORT: p,\n    SW_AI_BLOCKLIST_URL: '',", 1)
	if s := findHarnessBootSites("tests/x.js", jsOK); len(s) != 1 || !blocklistAssign.MatchString(s[0].block) {
		t.Errorf("JS site with the var: got %+v", s)
	}
	mk := "run: build\n\tSW_DB_PATH=./d.db ./bin\n\nci: build\n\tSW_DB_PATH=x \\\n\t  SW_PORT=1 \\\n\t  ./bin &\n"
	s := findHarnessBootSites("Makefile", mk)
	if len(s) != 1 || s[0].line != 5 || blocklistAssign.MatchString(s[0].block) {
		t.Errorf("Makefile: got %+v, want only the ci target's site (run is exempt), missing the var", s)
	}
	sh := "FOO=1 \\\n  SW_AI_BLOCKLIST_URL=\"\" \\\n  SW_DB_PATH=/tmp/x \\\n  ./bin &\n"
	if s := findHarnessBootSites("scripts/x.sh", sh); len(s) != 1 || !blocklistAssign.MatchString(s[0].block) {
		t.Errorf("shell: the var set earlier in the same continued command must count: %+v", s)
	}
}
