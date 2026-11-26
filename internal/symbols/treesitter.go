//go:build cgo

package symbols

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/c"
	"github.com/smacker/go-tree-sitter/cpp"
	"github.com/smacker/go-tree-sitter/csharp"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/java"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/kotlin"
	"github.com/smacker/go-tree-sitter/php"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/smacker/go-tree-sitter/ruby"
	"github.com/smacker/go-tree-sitter/rust"
	"github.com/smacker/go-tree-sitter/scala"
	"github.com/smacker/go-tree-sitter/swift"
	"github.com/smacker/go-tree-sitter/typescript/tsx"
	"github.com/smacker/go-tree-sitter/typescript/typescript"
)

// Available reports whether symbol analysis is compiled in.
func Available() bool { return true }

type grammar struct {
	name string
	lang func() *sitter.Language
	// defs maps definition node types to a kind.
	defs map[string]string
	// scopes are node types whose name qualifies nested definitions
	// (classes, impl blocks, modules) even if not emitted themselves.
	scopes map[string]bool
}

const (
	fn  = "function"
	mth = "method"
	typ = "type"
)

var jsDefs = map[string]string{
	"function_declaration": fn, "generator_function_declaration": fn, "method_definition": mth,
	"class_declaration": typ, "abstract_class_declaration": typ, "interface_declaration": typ,
	"type_alias_declaration": typ, "enum_declaration": typ, "variable_declarator": fn,
}

var grammars = map[string]*grammar{
	"go": {name: "go", lang: golang.GetLanguage, defs: map[string]string{
		"function_declaration": fn, "method_declaration": mth, "type_spec": typ}},
	"rust": {name: "rust", lang: rust.GetLanguage, defs: map[string]string{
		"function_item": fn, "struct_item": typ, "enum_item": typ, "trait_item": typ, "union_item": typ,
		"type_item": typ, "mod_item": typ, "macro_definition": fn},
		scopes: map[string]bool{"impl_item": true}},
	"python": {name: "python", lang: python.GetLanguage, defs: map[string]string{
		"function_definition": fn, "class_definition": typ}},
	"javascript": {name: "javascript", lang: javascript.GetLanguage, defs: jsDefs},
	"typescript": {name: "typescript", lang: typescript.GetLanguage, defs: jsDefs},
	"tsx":        {name: "tsx", lang: tsx.GetLanguage, defs: jsDefs},
	"java": {name: "java", lang: java.GetLanguage, defs: map[string]string{
		"method_declaration": mth, "constructor_declaration": mth, "class_declaration": typ,
		"interface_declaration": typ, "enum_declaration": typ, "record_declaration": typ, "annotation_type_declaration": typ}},
	"c": {name: "c", lang: c.GetLanguage, defs: map[string]string{
		"function_definition": fn, "struct_specifier": typ, "enum_specifier": typ, "union_specifier": typ}},
	"cpp": {name: "cpp", lang: cpp.GetLanguage, defs: map[string]string{
		"function_definition": fn, "class_specifier": typ, "struct_specifier": typ, "enum_specifier": typ},
		scopes: map[string]bool{"namespace_definition": true}},
	"csharp": {name: "csharp", lang: csharp.GetLanguage, defs: map[string]string{
		"method_declaration": mth, "constructor_declaration": mth, "class_declaration": typ, "interface_declaration": typ,
		"struct_declaration": typ, "enum_declaration": typ, "record_declaration": typ, "property_declaration": mth}},
	"ruby": {name: "ruby", lang: ruby.GetLanguage, defs: map[string]string{
		"method": mth, "singleton_method": mth, "class": typ, "module": typ}},
	"kotlin": {name: "kotlin", lang: kotlin.GetLanguage, defs: map[string]string{
		"function_declaration": fn, "class_declaration": typ, "object_declaration": typ}},
	"php": {name: "php", lang: php.GetLanguage, defs: map[string]string{
		"function_definition": fn, "method_declaration": mth, "class_declaration": typ,
		"interface_declaration": typ, "trait_declaration": typ}},
	"swift": {name: "swift", lang: swift.GetLanguage, defs: map[string]string{
		"function_declaration": fn, "class_declaration": typ, "protocol_declaration": typ}},
	"scala": {name: "scala", lang: scala.GetLanguage, defs: map[string]string{
		"function_definition": fn, "class_definition": typ, "object_definition": typ, "trait_definition": typ}},
}

var extensions = map[string]string{
	".go": "go", ".rs": "rust", ".py": "python", ".pyi": "python",
	".js": "javascript", ".mjs": "javascript", ".cjs": "javascript", ".jsx": "javascript",
	".ts": "typescript", ".mts": "typescript", ".cts": "typescript", ".tsx": "tsx",
	".java": "java", ".c": "c", ".h": "c",
	".cc": "cpp", ".cpp": "cpp", ".cxx": "cpp", ".hpp": "cpp", ".hh": "cpp", ".hxx": "cpp",
	".cs": "csharp", ".rb": "ruby", ".kt": "kotlin", ".kts": "kotlin", ".php": "php",
	".swift": "swift", ".scala": "scala", ".sc": "scala",
}

func grammarFor(path string) *grammar {
	return grammars[extensions[strings.ToLower(filepath.Ext(path))]]
}

// Supported reports whether path has a grammar.
func Supported(path string) bool { return grammarFor(path) != nil }

// Language returns the grammar name for path, or "".
func Language(path string) string {
	if g := grammarFor(path); g != nil {
		return g.name
	}
	return ""
}

// Extract parses src and returns its definitions.
func Extract(path string, src []byte) ([]Symbol, error) {
	g := grammarFor(path)
	if g == nil {
		return nil, ErrUnsupported
	}
	p := sitter.NewParser()
	defer p.Close()
	p.SetLanguage(g.lang())
	tree, err := p.ParseCtx(context.Background(), nil, src)
	if err != nil {
		return nil, err
	}
	defer tree.Close()
	var out []Symbol
	var walk func(n *sitter.Node, container string, depth int)
	walk = func(n *sitter.Node, container string, depth int) {
		if depth > 400 {
			return
		}
		t := n.Type()
		next := container
		if kind, ok := g.defs[t]; ok {
			if name := defName(g, n, src); name != "" {
				k := kind
				cont := container
				if g.name == "go" && t == "method_declaration" {
					cont = goReceiver(n, src)
				}
				if k == fn && cont != "" {
					k = mth
				}
				out = append(out, Symbol{
					Name: name, Kind: k, Container: cont,
					StartLine: int(n.StartPoint().Row) + 1, EndLine: int(n.EndPoint().Row) + 1,
				})
				if kind == typ {
					next = joinScope(container, name)
				}
			}
		} else if g.scopes[t] {
			if name := scopeName(n, src); name != "" {
				next = joinScope(container, name)
			}
		}
		for i := 0; i < int(n.NamedChildCount()); i++ {
			walk(n.NamedChild(i), next, depth+1)
		}
	}
	walk(tree.RootNode(), "", 0)
	return out, nil
}

func joinScope(a, b string) string {
	if a == "" {
		return b
	}
	return a + "." + b
}

var identTypes = map[string]bool{
	"identifier": true, "type_identifier": true, "simple_identifier": true, "constant": true,
	"name": true, "field_identifier": true, "property_identifier": true,
}

func defName(g *grammar, n *sitter.Node, src []byte) string {
	if n.Type() == "variable_declarator" {
		// Only arrow functions / function expressions bound to a name.
		v := n.ChildByFieldName("value")
		if v == nil {
			return ""
		}
		switch v.Type() {
		case "arrow_function", "function", "function_expression", "generator_function":
		default:
			return ""
		}
	}
	if c := n.ChildByFieldName("name"); c != nil {
		return strings.TrimSpace(c.Content(src))
	}
	if d := n.ChildByFieldName("declarator"); d != nil {
		return declaratorName(d, src)
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		if c := n.NamedChild(i); identTypes[c.Type()] {
			return c.Content(src)
		}
	}
	return ""
}

// declaratorName unwraps C/C++ declarators to the declared identifier.
func declaratorName(d *sitter.Node, src []byte) string {
	for i := 0; d != nil && i < 20; i++ {
		switch d.Type() {
		case "identifier", "field_identifier", "qualified_identifier", "destructor_name", "operator_name", "type_identifier":
			return d.Content(src)
		}
		next := d.ChildByFieldName("declarator")
		if next == nil && d.NamedChildCount() > 0 {
			next = d.NamedChild(0)
		}
		d = next
	}
	return ""
}

func scopeName(n *sitter.Node, src []byte) string {
	for _, f := range []string{"name", "type"} {
		if c := n.ChildByFieldName(f); c != nil {
			return strings.TrimSpace(c.Content(src))
		}
	}
	return ""
}

func goReceiver(n *sitter.Node, src []byte) string {
	r := n.ChildByFieldName("receiver")
	if r == nil {
		return ""
	}
	var find func(*sitter.Node) string
	find = func(x *sitter.Node) string {
		if x.Type() == "type_identifier" {
			return x.Content(src)
		}
		for i := 0; i < int(x.NamedChildCount()); i++ {
			if s := find(x.NamedChild(i)); s != "" {
				return s
			}
		}
		return ""
	}
	return find(r)
}
