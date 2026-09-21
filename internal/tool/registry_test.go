package tool

import (
	"context"
	"testing"
)

func TestCalculator_Basic(t *testing.T) {
	cases := []struct {
		expr string
		want string
	}{
		{"1+2", "3"},
		{"(12+34)*5/2", "115"},
		{"7*6", "42"},
		{" 10 / 4 ", "2.5"},
		{"-3+10", "7"},
		{"2*3+4*5", "26"},
	}
	calc := Calculator{}
	for _, c := range cases {
		got, err := calc.Call(context.Background(), c.expr)
		if err != nil {
			t.Fatalf("%s: unexpected error %v", c.expr, err)
		}
		if got != c.want {
			t.Fatalf("%s: got %s want %s", c.expr, got, c.want)
		}
	}
}

func TestCalculator_Invalid(t *testing.T) {
	calc := Calculator{}
	for _, expr := range []string{"1+", "abc", "1/0", "2**3", "1; rm -rf /"} {
		if _, err := calc.Call(context.Background(), expr); err == nil {
			t.Fatalf("expected error for %q", expr)
		}
	}
}

func TestTimeTool(t *testing.T) {
	out, err := TimeTool{}.Call(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) < 10 {
		t.Fatalf("unexpected time output: %s", out)
	}
}

func TestCodeRunner(t *testing.T) {
	cases := []struct {
		expr string
		want string
	}{
		{"sum([1,2,3])", "6"},
		{"avg([1,2,3,4])", "2.5"},
		{"max([3,9,2])", "9"},
		{"min([3,9,2])", "2"},
	}
	for _, c := range cases {
		got, err := CodeRunner{}.Call(context.Background(), c.expr)
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		if got != c.want {
			t.Fatalf("%s: got %s want %s", c.expr, got, c.want)
		}
	}
}

func TestRegistry(t *testing.T) {
	r := NewRegistry(Calculator{}, TimeTool{})
	if _, ok := r.Get("calculator"); !ok {
		t.Fatal("calculator not registered")
	}
	if len(r.Names()) != 2 {
		t.Fatalf("expected 2 tools, got %v", r.Names())
	}
	if _, ok := r.Get("nonexistent"); ok {
		t.Fatal("nonexistent tool should not be found")
	}
}
