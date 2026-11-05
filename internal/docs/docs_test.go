package docs_test

import (
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestDocs guards the documentation and CI contract in plan §3.17 and §5: the
// README has a quick start and points at the checklist, the two operator guides
// exist, every relative Markdown link resolves, the CI workflow defines one job
// per repository, and no go.work file is committed.
func TestDocs(t *testing.T) {
	root := repoRoot(t)

	t.Run("readme", func(t *testing.T) {
		readme := readFile(t, filepath.Join(root, "README.md"))

		if !strings.Contains(strings.ToLower(readme), "quick start") {
			t.Error("README.md has no quick start section")
		}
		if !strings.Contains(readme, "go build") && !strings.Contains(readme, "go run") {
			t.Error("README.md quick start has no build or run command")
		}

		var linkedChecklist bool
		for _, target := range markdownLinks(readme) {
			if strings.TrimPrefix(target, "./") == "checklist.md" {
				linkedChecklist = true
				break
			}
		}
		if !linkedChecklist {
			t.Error("README.md does not link to checklist.md")
		}
	})

	t.Run("guides", func(t *testing.T) {
		for _, name := range []string{
			filepath.Join("docs", "adding-a-satellite.md"),
			filepath.Join("docs", "architecture.md"),
		} {
			info, err := os.Stat(filepath.Join(root, name))
			if err != nil {
				t.Errorf("%s: %v", name, err)
				continue
			}
			if info.Size() == 0 {
				t.Errorf("%s is empty", name)
			}
		}
	})

	t.Run("process docs", func(t *testing.T) {
		for _, name := range []string{"process.md", "amendments.md"} {
			info, err := os.Stat(filepath.Join(root, name))
			if err != nil {
				t.Errorf("%s: %v", name, err)
				continue
			}
			if info.Size() == 0 {
				t.Errorf("%s is empty", name)
			}
		}

		for _, name := range []string{"checklist.md", "amendments.md"} {
			if !linksTo(readFile(t, filepath.Join(root, name)), "process.md") {
				t.Errorf("%s does not link to process.md", name)
			}
		}

		amendments := readFile(t, filepath.Join(root, "amendments.md"))
		for _, field := range []string{"**Amends:**", "**Why:**"} {
			if !strings.Contains(amendments, field) {
				t.Errorf("amendments.md has no %s field", field)
			}
		}
	})

	t.Run("relative links resolve", func(t *testing.T) {
		for _, path := range markdownFiles(t, root) {
			base := filepath.Dir(path)
			for _, target := range markdownLinks(readFile(t, path)) {
				rel, ok := localLink(target)
				if !ok {
					continue
				}
				if strings.HasPrefix(rel, "/") {
					rel = strings.TrimPrefix(rel, "/")
				} else {
					rel = filepath.Join(base, rel)
				}
				if _, err := os.Stat(filepath.Clean(rel)); err != nil {
					from, _ := filepath.Rel(root, path)
					t.Errorf("%s: link %q does not resolve: %v", from, target, err)
				}
			}
		}
	})

	t.Run("ci workflow", func(t *testing.T) {
		data, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
		if err != nil {
			t.Fatalf("read CI workflow: %v", err)
		}

		var doc struct {
			Jobs map[string]yaml.Node `yaml:"jobs"`
		}
		if err := yaml.Unmarshal(data, &doc); err != nil {
			t.Fatalf("parse CI workflow: %v", err)
		}

		for _, name := range []string{"aether", "msg", "wind-predictor"} {
			job, ok := doc.Jobs[name]
			if !ok {
				t.Errorf("CI workflow has no %q job", name)
				continue
			}
			var body map[string]any
			if err := job.Decode(&body); err != nil {
				t.Errorf("CI job %q is not a mapping: %v", name, err)
				continue
			}
			if steps, ok := body["steps"].([]any); !ok || len(steps) == 0 {
				t.Errorf("CI job %q has no steps", name)
			}
		}
	})

	t.Run("no go.work committed", func(t *testing.T) {
		for _, name := range []string{"go.work", "go.work.sum"} {
			if _, err := os.Stat(filepath.Join(root, name)); err == nil {
				t.Errorf("%s exists in the working tree; CI generates it only", name)
			}
		}

		cmd := exec.Command("git", "-C", root, "ls-files", "--", "go.work", "go.work.sum")
		out, err := cmd.Output()
		if err == nil && strings.TrimSpace(string(out)) != "" {
			t.Errorf("go.work is committed: %q", strings.TrimSpace(string(out)))
		}
	})
}

func repoRoot(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root, err := filepath.Abs(filepath.Join(filepath.Dir(file), "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	return root
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func markdownFiles(t *testing.T, root string) []string {
	t.Helper()

	var files []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "vendor", "node_modules", "bin", "data":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.EqualFold(filepath.Ext(path), ".md") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk Markdown files: %v", err)
	}
	return files
}

var (
	fencedCode = regexp.MustCompile("(?s)```.*?```|~~~.*?~~~")
	inlineCode = regexp.MustCompile("`[^`\n]*`")
	linkTarget = regexp.MustCompile(`!?\[[^\]]*\]\(([^)\n]+)\)`)
)

// markdownLinks returns the raw targets of inline Markdown links and images,
// ignoring anything inside fenced or inline code.
func markdownLinks(text string) []string {
	text = fencedCode.ReplaceAllString(text, "")
	text = inlineCode.ReplaceAllString(text, "")

	var targets []string
	for _, match := range linkTarget.FindAllStringSubmatch(text, -1) {
		target := strings.TrimSpace(match[1])
		if strings.HasPrefix(target, "<") && strings.HasSuffix(target, ">") {
			target = strings.TrimSuffix(strings.TrimPrefix(target, "<"), ">")
		}
		// Drop an optional link title: [x](path "title").
		if i := strings.IndexAny(target, " \t"); i >= 0 {
			target = target[:i]
		}
		if target != "" {
			targets = append(targets, target)
		}
	}
	return targets
}

// linksTo reports whether text contains a relative Markdown link to target.
func linksTo(text, target string) bool {
	for _, raw := range markdownLinks(text) {
		if rel, ok := localLink(raw); ok && filepath.ToSlash(rel) == target {
			return true
		}
	}
	return false
}

// localLink reports whether target is a repository-relative file reference and
// returns it without its fragment or query. External URLs, absolute-site links,
// and pure anchors are skipped.
func localLink(target string) (string, bool) {
	if target == "" || strings.HasPrefix(target, "#") {
		return "", false
	}
	lower := strings.ToLower(target)
	for _, prefix := range []string{"http://", "https://", "mailto:", "tel:", "data:"} {
		if strings.HasPrefix(lower, prefix) {
			return "", false
		}
	}
	if strings.Contains(target, "://") {
		return "", false
	}

	if i := strings.IndexAny(target, "#?"); i >= 0 {
		target = target[:i]
	}
	if target == "" {
		return "", false
	}
	if decoded, err := url.PathUnescape(target); err == nil {
		target = decoded
	}
	return target, true
}
