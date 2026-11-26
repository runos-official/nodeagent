package certpolicy

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// allowedImports is the whole import list a non-test file of the package
// may use. The package reads no file, no network, no clock and no random
// source of its own.
var allowedImports = map[string]bool{
	"crypto/sha256":                 true,
	"encoding/binary":               true,
	"errors":                        true,
	"strconv":                       true,
	"strings":                       true,
	"time":                          true,
	"unicode/utf8":                  true,
	"google.golang.org/grpc/codes":  true,
	"google.golang.org/grpc/status": true,
	"google.golang.org/genproto/googleapis/rpc/errdetails": true,
}

func TestPackageIsPure(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var seen []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		af, err := parser.ParseFile(fset, f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			seen = append(seen, f+": "+p)
			if strings.HasPrefix(p, "github.com/runos-official/nodeagent") {
				t.Errorf("%s imports a NodeAgent package: %s", f, p)
			}
			switch p {
			case "os", "net", "math/rand", "math/rand/v2", "crypto/rand":
				t.Errorf("%s imports %s", f, p)
			}
			if strings.HasPrefix(p, "net/") || strings.HasPrefix(p, "os/") {
				t.Errorf("%s imports %s", f, p)
			}
			if !allowedImports[p] {
				t.Errorf("%s imports %s, which is not on the allowed list", f, p)
			}
		}
	}
	if len(seen) == 0 {
		t.Fatal("no import read: the test reads the wrong directory")
	}
	sort.Strings(seen)
	t.Log(strings.Join(seen, "\n"))
}

// TestNoClockRead keeps time.Now out of the package: the caller passes now.
func TestNoClockRead(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"time.Now(", "time.Since(", "time.Until("} {
			if strings.Contains(string(src), bad) {
				t.Errorf("%s calls %s", f, bad)
			}
		}
	}
}
