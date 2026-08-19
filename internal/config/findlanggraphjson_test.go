package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindLangGraphJSON_ExplicitFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "langgraph.json")
	if err := os.WriteFile(path, []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}
	got := FindLangGraphJSON(path)
	if len(got) != 1 || got[0] != path {
		t.Fatalf("expected [%q], got %v", path, got)
	}
}

func TestFindLangGraphJSON_ExplicitDirGlobsAllJSONSorted(t *testing.T) {
	dir := t.TempDir()
	// Written out of order to prove the result is sorted, not readdir order.
	names := []string{"worker.json", "coordinator.json", "aux.json"}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(`{}`), 0644); err != nil {
			t.Fatal(err)
		}
	}
	// Non-JSON file in the same dir must be ignored by the glob.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}

	got := FindLangGraphJSON(dir)
	want := []string{
		filepath.Join(dir, "aux.json"),
		filepath.Join(dir, "coordinator.json"),
		filepath.Join(dir, "worker.json"),
	}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected %v, got %v", want, got)
		}
	}
}

func TestFindLangGraphJSON_ExplicitDirWithNoJSONReturnsNil(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := FindLangGraphJSON(dir); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

func TestFindLangGraphJSON_ExplicitNonexistentPathReturnsNil(t *testing.T) {
	if got := FindLangGraphJSON(filepath.Join(t.TempDir(), "does-not-exist")); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

func TestFindLangGraphJSON_EmptyExplicitFallsBackToCWD(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, "langgraph.json")
	if err := os.WriteFile(path, []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}
	got := FindLangGraphJSON("")
	if len(got) != 1 || got[0] != "langgraph.json" {
		t.Fatalf("expected [\"langgraph.json\"], got %v", got)
	}
}

func TestFindLangGraphJSON_EmptyExplicitFallsBackToExamplesGlob(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	examplePath := filepath.Join(dir, "examples", "demo_agent", "langgraph.json")
	if err := os.MkdirAll(filepath.Dir(examplePath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(examplePath, []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}
	got := FindLangGraphJSON("")
	if len(got) != 1 || got[0] != filepath.Join("examples", "demo_agent", "langgraph.json") {
		t.Fatalf("expected [%q], got %v", filepath.Join("examples", "demo_agent", "langgraph.json"), got)
	}
}
