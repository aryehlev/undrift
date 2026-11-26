//go:build cgo

package symbols

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

func names(syms []Symbol) []string {
	var out []string
	for _, s := range syms {
		out = append(out, s.Kind+":"+s.Qualified())
	}
	sort.Strings(out)
	return out
}

func TestExtractLanguages(t *testing.T) {
	cases := map[string]struct {
		src  string
		want []string
	}{
		"a.go": {"package a\n\nfunc RunAuction() {}\n\ntype Engine struct{}\n\nfunc (e *Engine) Execute() {}\n",
			[]string{"function:RunAuction", "method:Engine.Execute", "type:Engine"}},
		"a.rs": {"pub struct Engine;\nimpl Engine {\n    pub fn execute(&self) {}\n}\nfn helper() {}\n",
			[]string{"function:helper", "method:Engine.execute", "type:Engine"}},
		"a.py": {"class Engine:\n    def execute(self):\n        pass\n\ndef run_auction():\n    pass\n",
			[]string{"function:run_auction", "method:Engine.execute", "type:Engine"}},
		"a.ts": {"export class Engine {\n  execute(): void {}\n}\nexport const runAuction = () => 1;\ninterface Bid { price: number }\n",
			[]string{"function:runAuction", "method:Engine.execute", "type:Bid", "type:Engine"}},
		"A.java": {"class Engine {\n  void execute() {}\n  Engine() {}\n}\n",
			[]string{"method:Engine.Engine", "method:Engine.execute", "type:Engine"}},
		"a.c": {"struct bid { int p; };\nstatic int run_auction(int n) { return n; }\n",
			[]string{"function:run_auction", "type:bid"}},
		"a.rb": {"class Engine\n  def execute\n  end\nend\n",
			[]string{"method:Engine.execute", "type:Engine"}},
	}
	for path, c := range cases {
		syms, err := Extract(path, []byte(c.src))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if got := names(syms); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v want %v", path, got, c.want)
		}
	}
	if Supported("README.md") {
		t.Fatal("markdown should be unsupported")
	}
}

func TestChangedInnermost(t *testing.T) {
	old := "package a\n\nimport \"fmt\"\n\ntype E struct{}\n\nfunc (e E) A() {\n\tfmt.Println(1)\n}\n\nfunc B() {\n}\n"
	neu := "package a\n\nimport \"fmt\"\n\ntype E struct{}\n\nfunc (e E) A() {\n\tfmt.Println(2)\n}\n\nfunc C() {\n}\n"
	// line 8 changed (A body); B (11-12) deleted and C added.
	fc, err := Changed("a.go", []byte(old), []byte(neu), []int{8, 11, 12}, []int{8, 11, 12})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fc.Symbols, []string{"B", "C", "E.A"}) || fc.TopLevel {
		t.Fatalf("%+v", fc)
	}
	fc, _ = Changed("a.go", []byte(old), []byte(neu), nil, []int{3})
	if !fc.TopLevel || len(fc.Symbols) != 0 {
		t.Fatalf("import change: %+v", fc)
	}
}

func TestExcerpt(t *testing.T) {
	src := []byte("package a\n\nfunc A() {\n\treturn\n}\n\nfunc B() {}\n\nfunc Z() {}\n")
	syms, _ := Extract("a.go", src)
	ex := Excerpt(src, Find(syms, "B"), 0)
	if !strings.Contains(ex, "func B()") || strings.Contains(ex, "func A()") || !strings.Contains(ex, "omitted") {
		t.Fatalf("excerpt:\n%s", ex)
	}
}
