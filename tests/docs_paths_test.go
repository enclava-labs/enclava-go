package tests

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestLivingDocumentationPaths(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	root := os.DirFS(filepath.Dir(filepath.Dir(filename)))
	tokens := regexp.MustCompile("`([^`\\n]+)`")
	prefixes := []string{"internal/", "cmd/", "docs/", "scripts/", "tests/", ".planning/", ".github/"}
	// Historical planning documents describe past snapshots, not the current tree.
	for _, doc := range []string{"docs/api.md", "docs/go-rewrite-contract.md", "docs/cross-cutting.md"} {
		content, err := fs.ReadFile(root, doc)
		if err != nil {
			t.Errorf("read %s: %v", doc, err)
			continue
		}
		for _, match := range tokens.FindAllStringSubmatch(string(content), -1) {
			ref := match[1]
			if strings.ContainsAny(ref, "*?<>[]{} \t\r\n") {
				continue
			}
			isPath := false
			for _, prefix := range prefixes {
				isPath = isPath || strings.HasPrefix(ref, prefix)
			}
			if !isPath {
				continue
			}
			if index := strings.IndexAny(ref, ":#"); index >= 0 {
				ref = ref[:index]
			}
			cleaned := path.Clean(ref)
			if !fs.ValidPath(cleaned) {
				continue
			}
			if _, err := fs.Stat(root, cleaned); err != nil {
				t.Errorf("%s references %s: %v", doc, cleaned, err)
			}
		}
	}
}
