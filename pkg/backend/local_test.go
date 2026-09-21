package backend

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestLocalList(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"dists/stable/Release":                         "Origin: test\n",
		"dists/stable/main/binary-amd64/Packages":      "Package: hello\n",
		"pool/main/h/hello/hello_2.10-3_amd64.deb":     "hello",
		"pool/main/libf/libfoo/libfoo_1.3.0-1_all.deb": "libfoo",
		"createapt-go.json":                            "{}",
	}
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	be, err := newLocal(dir)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("whole repo", func(t *testing.T) {
		objs, err := be.List(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		got := paths(objs)
		want := []string{
			"createapt-go.json",
			"dists/stable/Release",
			"dists/stable/main/binary-amd64/Packages",
			"pool/main/h/hello/hello_2.10-3_amd64.deb",
			"pool/main/libf/libfoo/libfoo_1.3.0-1_all.deb",
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("List(\"\") = %v; want %v", got, want)
		}
		// Sizes must be reported.
		for _, o := range objs {
			if o.Size == 0 {
				t.Errorf("object %s has zero size", o.Path)
			}
		}
	})

	t.Run("scoped to dists", func(t *testing.T) {
		objs, err := be.List(context.Background(), "dists/")
		if err != nil {
			t.Fatal(err)
		}
		got := paths(objs)
		want := []string{"dists/stable/Release", "dists/stable/main/binary-amd64/Packages"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("List(\"dists/\") = %v; want %v", got, want)
		}
	})

	t.Run("missing prefix is empty", func(t *testing.T) {
		objs, err := be.List(context.Background(), "does-not-exist/")
		if err != nil {
			t.Fatalf("List of missing prefix errored: %v", err)
		}
		if len(objs) != 0 {
			t.Errorf("List of missing prefix = %v; want empty", paths(objs))
		}
	})
}

func paths(objs []ObjectInfo) []string {
	out := make([]string, len(objs))
	for i, o := range objs {
		out[i] = o.Path
	}
	sort.Strings(out)
	return out
}
