package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestProductionModelSourceHasNoFixedIDs(t *testing.T) {
	banned := []string{
		"glm-5.3", "glm-5.3-flash", "glm-5.2", "glm-5.1", "glm-5v-turbo",
		"kimi-k2.6", "kimi-k3-1", "kimi-k2.7", "minimax-m3",
		"hy3", "hy3-x", "hy3-preview", "hy3-preview-agent",
		"hy4-preview", "hy4-preview-x", "deepseek-v4-pro", "deepseek-v4-flash",
		"forceMaxThinking",
	}
	// activity.go carries one model ID on purpose: it is the identifier inside
	// the client's chat_request_send telemetry payload (the growth system keys
	// its counters off it), not something this plugin routes to. Routing stays
	// fully dynamic; see the note in activity.go. Only these exact literals are
	// exempt, and only in that file — any other ID anywhere is a hard failure.
	exempt := map[string][]string{
		"activity.go": {"deepseek-v4-flash", "glm-5.2"},
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range banned {
			if !bytes.Contains(raw, []byte(value)) {
				continue
			}
			if slices.Contains(exempt[name], value) {
				continue
			}
			t.Errorf("production file %s contains banned model contract %q", name, value)
		}
	}
}

func TestProductionModelInfoLiteralsOnlyNameAuto(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			literal, ok := node.(*ast.CompositeLit)
			if !ok {
				return true
			}
			selector, ok := literal.Type.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "ModelInfo" {
				return true
			}
			for _, element := range literal.Elts {
				pair, ok := element.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := pair.Key.(*ast.Ident)
				value, literalID := pair.Value.(*ast.BasicLit)
				if !ok || key.Name != "ID" || !literalID || value.Kind != token.STRING {
					continue
				}
				id, err := strconv.Unquote(value.Value)
				if err != nil || id != "auto" {
					t.Errorf("production file %s has static ModelInfo ID %s", name, value.Value)
				}
			}
			return true
		})
	}
}

func TestProductionMetadataHasOnlyDefaultTemplate(t *testing.T) {
	got := modelInfoFromFacts(modelFacts{ID: "serve-alpha"})
	want := defaultModelInfo("serve-alpha", "")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unmatched dynamic metadata = %#v, want %#v", got, want)
	}
}
