package adapter_test

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// forbiddenDiskImports names the packages that actually touch the local
// filesystem (open/read/write/stat/remove a real file). Only
// internal/media/adapter may import them — domain and app must reach the
// filesystem exclusively through the domain.PhotoStore port, never directly,
// so a second backend (NES-132's object store) is a pure adapter swap with
// no domain/app change required.
//
// "path/filepath" is deliberately NOT included here: internal/media/app's
// photo_service.go imports it for filepath.Ext on an already-opaque
// StorageRef string (contentTypeForRef) — pure string manipulation that
// never resolves or touches an actual location on disk. Flagging that import
// would be a false positive; "os" is the package whose functions actually
// perform disk I/O, so it is the precise signal for a hexagonal-boundary
// violation.
var forbiddenDiskImports = map[string]bool{
	"os": true,
}

// TestNoDiskImportsOutsideAdapter covers AC3 by construction: it parses every
// non-test .go file in internal/media/domain and internal/media/app and
// fails if any of them imports a filesystem-touching package, reporting the
// offending file:line. Paths are resolved relative to this test file's own
// location (runtime.Caller) so the check is independent of the working
// directory `go test` runs from.
func TestNoDiskImportsOutsideAdapter(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed to resolve this test file's path")
	}
	adapterDir := filepath.Dir(thisFile)
	mediaDir := filepath.Dir(adapterDir)

	for _, sub := range []string{"domain", "app"} {
		dir := filepath.Join(mediaDir, sub)
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatalf("glob %s: %v", dir, err)
		}
		if len(files) == 0 {
			t.Fatalf("no .go files found under %s; the boundary check found nothing to verify", dir)
		}
		for _, path := range files {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			checkFileImports(t, path)
		}
	}
}

func checkFileImports(t *testing.T, path string) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, imp := range file.Imports {
		importPath, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			t.Fatalf("%s: unquote import path %s: %v", path, imp.Path.Value, err)
		}
		if forbiddenDiskImports[importPath] {
			pos := fset.Position(imp.Pos())
			t.Errorf("%s:%d: package outside internal/media/adapter imports %q; disk access must stay behind the PhotoStore port", pos.Filename, pos.Line, importPath)
		}
	}
}
