package internal_test

import (
	"errors"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePrefix = "github.com/fabriciossouza/poc-vtex-seller-go/internal/"

// allowed lists, per layer, the internal packages it may import. A layer
// applies to every package under it unless a more specific layer is listed.
var allowed = map[string][]string{
	"domain": {},
	"ports":  {"domain"},
	"app":    {"domain", "ports"},
	// Driven adapters may not import each other or app: they are wired in main.
	"adapters": {"domain", "ports"},
	// The HTTP adapter drives the application, so it calls the use cases.
	"adapters/http": {"domain", "ports", "app"},
	// Platform code is wired in main; domain, ports and app never import it.
	"platform":            {"domain", "ports", "app", "platform/resilience"},
	"platform/resilience": {},
}

func TestLayersOnlyImportAllowedInternalPackages(t *testing.T) {
	for layer, allow := range allowed {
		files := goSources(t, layer)
		if len(files) == 0 && layer != "adapters" {
			t.Fatalf("no sources found for layer %s", layer)
		}
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") || mostSpecificLayer(file) != layer {
				continue
			}
			src, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			f, err := parser.ParseFile(token.NewFileSet(), file, src, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("parse %s: %v", file, err)
			}
			for _, imp := range f.Imports {
				path, _ := strconv.Unquote(imp.Path.Value)
				pkg, ok := strings.CutPrefix(path, modulePrefix)
				if !ok {
					continue
				}
				if !contains(allow, pkg) {
					t.Errorf("%s imports internal/%s; layer %q may import only %v", file, pkg, layer, allow)
				}
			}
		}
	}
}

func mostSpecificLayer(file string) string {
	best := ""
	for layer := range allowed {
		if strings.HasPrefix(file, layer+"/") && len(layer) > len(best) {
			best = layer
		}
	}
	return best
}

func goSources(t *testing.T, layer string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(layer, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".go") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return files
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
