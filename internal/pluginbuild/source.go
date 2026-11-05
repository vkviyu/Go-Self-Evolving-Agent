package pluginbuild

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
)

var allowedImports = map[string]bool{
	"encoding/json": true, "strings": true, "bytes": true, "strconv": true, "math": true,
	"sort": true, "unicode": true, "unicode/utf8": true, "errors": true, "fmt": true, "regexp": true, "slices": true, "cmp": true,
}

func ValidateSource(source string) error {
	if len(source) == 0 || len(source) > 64<<10 {
		return errors.New("candidate Go source must be between 1 byte and 64 KiB")
	}
	file, err := parser.ParseFile(token.NewFileSet(), "candidate.go", source, parser.ParseComments|parser.AllErrors)
	if err != nil {
		return err
	}
	if file.Name.Name != "main" {
		return errors.New("candidate package must be main")
	}
	for _, group := range file.Comments {
		for _, comment := range group.List {
			if strings.Contains(comment.Text, "go:") {
				return errors.New("compiler directives are reserved to the trusted SDK")
			}
		}
	}
	for _, item := range file.Imports {
		path, err := strconv.Unquote(item.Path.Value)
		if err != nil {
			return err
		}
		if !allowedImports[path] || item.Name != nil {
			return errors.New("candidate imports must use approved standard-library packages without aliases")
		}
	}
	found := false
	for _, decl := range file.Decls {
		if function, ok := decl.(*ast.FuncDecl); ok {
			if function.Name.Name == "init" || function.Name.Name == "main" || function.Name.Name == "pluginInvoke" || function.Name.Name == "pluginABIVersion" {
				return errors.New("candidate cannot replace initialization or SDK entrypoints")
			}
			if function.Name.Name == "Invoke" && function.Recv == nil {
				found = true
			}
		}
	}
	if !found {
		return errors.New("candidate must define func Invoke(request json.RawMessage) (json.RawMessage, error)")
	}
	return nil
}
