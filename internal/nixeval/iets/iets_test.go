//go:build iets

package iets

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEvalJSON(t *testing.T) {
	out, err := Eval(`{ a = 1; b = [ 1 2 ]; c = "x" + "y"; }`, "x86_64-linux", "", t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		A int    `json:"a"`
		B []int  `json:"b"`
		C string `json:"c"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if v.A != 1 || len(v.B) != 2 || v.C != "xy" {
		t.Fatalf("unexpected value %s", out)
	}
}

func TestEvalError(t *testing.T) {
	_, err := Eval(`throw "nope"`, "x86_64-linux", "", t.TempDir(), 1)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Fatalf("error does not carry the message: %q", err)
	}
}

func TestVersion(t *testing.T) {
	if !strings.HasPrefix(Version(), "iets ") {
		t.Fatalf("version %q", Version())
	}
}
