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
	dbPathAssign = regexp.MustCompile(`SW_DB_PATH\s*[:=]`)
	// blocklistAssign matches an EMPTY assignment in every syntax the boot
	// sites use: `VAR= \` (make/shell continued), `VAR="" \`, `VAR: ""` (YAML)
	// and `VAR: '',` (JS). The value must be followed by a continuation, a
	// comma or the line end, so a real URL (`VAR="https://..."`) never matches.
	blocklistAssign = regexp.MustCompile(`(?m)SW_AI_BLOCKLIST_URL\s*[:=][ \t]*(?:""|'')?[ \t]*(?:\\|,|$)`)
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
	byIndent := false
	for _, ext := range []string{".yml", ".yaml", ".js", ".mjs", ".cjs", ".ts"} {
		byIndent = byIndent || strings.HasSuffix(name, ext)
	}
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
		// Comment lines are dropped, so a commented-out assignment cannot
		// satisfy the check.
		var code []string
		for _, bl := range lines[lo : hi+1] {
			if !isCommentLine(bl) {
				code = append(code, bl)
			}
		}
		sites = append(sites, harnessBootSite{file: name, line: i + 1, block: strings.Join(code, "\n")})
	}
	return sites
}

// harnessFiles lists the files a test-harness boot site can live in: the
// Makefile, every workflow, and every script or JS/TS file under scripts/ and
// tests/ at any depth (node_modules skipped). A boot site configured through
// SW_CONFIG_PATH TOML instead of SW_DB_PATH is not detected.
func harnessFiles(t *testing.T, root string) []string {
	t.Helper()
	files := []string{filepath.Join(root, "Makefile")}
	for _, g := range []string{".github/workflows/*.yml", ".github/workflows/*.yaml"} {
		m, err := filepath.Glob(filepath.Join(root, g))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	exts := map[string]bool{".sh": true, ".js": true, ".mjs": true, ".cjs": true, ".ts": true}
	for _, dir := range []string{"scripts", "tests"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			if !d.IsDir() && exts[filepath.Ext(p)] {
				files = append(files, p)
			}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
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

	// G2: a commented-out assignment does not count.
	yml := "        env:\n          SW_DB_PATH: /tmp/x.db\n          # SW_AI_BLOCKLIST_URL: \"\"\n          SW_PORT: 1\n"
	if s := findHarnessBootSites(".github/workflows/x.yml", yml); len(s) != 1 || blocklistAssign.MatchString(s[0].block) {
		t.Errorf("a commented-out SW_AI_BLOCKLIST_URL must not satisfy the check: %+v", s)
	}
	// G3: a non-empty value does not count, in each syntax.
	for name, body := range map[string]string{
		"scripts/x.sh":            "SW_AI_BLOCKLIST_URL=\"https://mirror.example/l.txt\" \\\n  SW_DB_PATH=/tmp/x \\\n  ./bin &\n",
		".github/workflows/x.yml": "        env:\n          SW_DB_PATH: /tmp/x.db\n          SW_AI_BLOCKLIST_URL: https://mirror.example/l.txt\n",
		"tests/x.js":              "  env: {\n    SW_DB_PATH: db,\n    SW_AI_BLOCKLIST_URL: 'https://mirror.example/l.txt',\n  },\n",
	} {
		if s := findHarnessBootSites(name, body); len(s) != 1 || blocklistAssign.MatchString(s[0].block) {
			t.Errorf("%s: a non-empty SW_AI_BLOCKLIST_URL must not satisfy the check: %+v", name, s)
		}
	}
	// Every empty syntax the real boot sites use does count.
	for _, ok := range []string{"SW_AI_BLOCKLIST_URL= \\", "SW_AI_BLOCKLIST_URL=\"\" \\", "SW_AI_BLOCKLIST_URL: \"\"", "SW_AI_BLOCKLIST_URL: '',", "X=1 SW_AI_BLOCKLIST_URL= \\"} {
		if !blocklistAssign.MatchString(ok) {
			t.Errorf("empty assignment %q must satisfy the check", ok)
		}
	}
}

// G4: the file walk reaches .mjs helpers and nested tests/**/*.sh scripts.
func TestHarnessFilesScanDepthAndExtensions(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Makefile", "")
	write("tests/e2e/helpers/boot.mjs", "  env: {\n    SW_DB_PATH: db,\n  },\n")
	write("tests/smoke/nested/boot.sh", "SW_DB_PATH=/tmp/x \\\n  ./bin &\n")
	write("tests/node_modules/pkg/boot.js", "SW_DB_PATH: x,\n")
	found := map[string]bool{}
	for _, f := range harnessFiles(t, root) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(root, f)
		for _, s := range findHarnessBootSites(rel, string(b)) {
			found[s.file] = true
		}
	}
	for _, want := range []string{"tests/e2e/helpers/boot.mjs", "tests/smoke/nested/boot.sh"} {
		if !found[want] {
			t.Errorf("boot site in %s not found; found %v", want, found)
		}
	}
	if found["tests/node_modules/pkg/boot.js"] {
		t.Error("node_modules must be skipped")
	}
}
