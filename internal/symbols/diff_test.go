package symbols

import (
	"reflect"
	"testing"
)

func TestParseUnifiedDiff(t *testing.T) {
	d := `diff --git a/auction/auction.go b/auction/auction.go
index 1..2 100644
--- a/auction/auction.go
+++ b/auction/auction.go
@@ -5,2 +5,3 @@ func RunAuction() {
@@ -20 +21,0 @@ func helper() {
diff --git a/new.go b/new.go
new file mode 100644
--- /dev/null
+++ b/new.go
@@ -0,0 +1,4 @@
diff --git a/old.go b/renamed.go
--- a/old.go
+++ b/renamed.go
@@ -3,0 +4,2 @@
`
	got := ParseUnifiedDiff(d)
	if l := got["auction/auction.go"]; !reflect.DeepEqual(l.Old, []int{5, 6, 20}) || !reflect.DeepEqual(l.New, []int{5, 6, 7, 21}) {
		t.Fatalf("auction: %+v", l)
	}
	if l := got["new.go"]; len(l.Old) != 0 || !reflect.DeepEqual(l.New, []int{1, 2, 3, 4}) {
		t.Fatalf("new: %+v", l)
	}
	if got["old.go"] != got["renamed.go"] || !reflect.DeepEqual(got["renamed.go"].Old, []int{3}) {
		t.Fatalf("rename: %+v", got["renamed.go"])
	}
}

func TestNormalize(t *testing.T) {
	for _, n := range []string{"RunAuction", "run_auction", "runAuction", "Engine.run_auction", "auction::RunAuction"} {
		if Normalize(n) != "runauction" {
			t.Errorf("Normalize(%q) = %q", n, Normalize(n))
		}
	}
}
