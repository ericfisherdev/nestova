package adapter_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"testing"
)

// forbiddenQualifiedCalls names package-qualified whole-file-read calls the
// upload path must never make: each defeats the streaming design (io.Copy in
// bounded chunks) AC4 requires, buffering an entire file into memory instead.
var forbiddenQualifiedCalls = map[string]map[string]bool{
	"io":     {"ReadAll": true},
	"ioutil": {"ReadAll": true},
	"os":     {"ReadFile": true},
}

// forbiddenMethodNames matches a call by selector name alone, regardless of
// receiver/package, since some whole-file-read APIs are methods rather than
// package-level functions — e.g. (*bytes.Buffer).ReadFrom, which (unlike
// io.Copy) drains its source in one call by growing its buffer as needed.
var forbiddenMethodNames = map[string]bool{
	"ReadFrom": true,
}

// uploadPathFiles are the files on the photo-upload receive/store hot path
// (web handler -> service -> store) that AC4 requires stay free of whole-file
// buffering. Paths are resolved relative to this test file's own location
// (via runtime.Caller) so the check is independent of the working directory
// `go test` happens to run from.
func uploadPathFiles(t *testing.T) []string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed to resolve this test file's path")
	}
	dir := filepath.Dir(thisFile)
	return []string{
		filepath.Join(dir, "photo_store.go"),
		filepath.Join(dir, "web.go"),
		filepath.Join(dir, "..", "app", "photo_service.go"),
	}
}

// TestUploadPathNeverBuffersWholeFile covers AC4 by construction: it parses
// the actual upload-path source files and fails if any of them calls a
// whole-file-read API, reporting the offending file:line. This is the
// authoritative check for the pattern the sibling chunk-size test
// (TestPutStreamsWithoutBufferingWholeFile) cannot reliably catch: an
// io.ReadAll-style caller drains its source across many individually
// small/bounded Read calls (growing an internal buffer as it goes), so
// sampling the single largest Read request size never sees one big read to
// flag — the anti-pattern is only visible in the call itself, not in any one
// Read's buffer size. A future change that reintroduces whole-file buffering
// on this path is caught here regardless of how it drains its source.
func TestUploadPathNeverBuffersWholeFile(t *testing.T) {
	fset := token.NewFileSet()
	for _, path := range uploadPathFiles(t) {
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pos := fset.Position(call.Pos())
			if forbiddenMethodNames[sel.Sel.Name] {
				t.Errorf("%s:%d: forbidden whole-file-read call %s(...) on the upload path; stream instead",
					pos.Filename, pos.Line, sel.Sel.Name)
				return true
			}
			pkgIdent, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if forbiddenQualifiedCalls[pkgIdent.Name][sel.Sel.Name] {
				t.Errorf("%s:%d: forbidden whole-file-read call %s.%s(...) on the upload path; stream instead",
					pos.Filename, pos.Line, pkgIdent.Name, sel.Sel.Name)
			}
			return true
		})
	}
}
