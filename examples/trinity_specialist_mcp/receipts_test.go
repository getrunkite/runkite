package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func receiptTestConfig(t *testing.T) bridgeConfig {
	t.Helper()
	return bridgeConfig{URL: "https://specialist.example", Agent: "specialist", Profile: "test-personal", Key: "synthetic-test-credential", State: receiptTestDirectory(t)}
}

func receiptTestDirectory(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	return directory
}

func receiptTestInvocation() invocation {
	return invocation{OperationID: "operation-1", ThreadID: "conversation-1", Message: "Summarize the public example."}
}

func TestReceiptImmutableBinding(t *testing.T) {
	cfg := receiptTestConfig(t)
	in := receiptTestInvocation()
	first, err := loadOrSaveReceipt(cfg, cfg.URL, in)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.State, receiptName(in.OperationID)+".json")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func(*bridgeConfig, *invocation)
	}{
		{"endpoint", func(c *bridgeConfig, _ *invocation) { c.URL = "https://another.example" }},
		{"profile", func(c *bridgeConfig, _ *invocation) { c.Profile = "another-identity" }},
		{"agent", func(c *bridgeConfig, _ *invocation) { c.Agent = "another-specialist" }},
		{"thread", func(_ *bridgeConfig, i *invocation) { i.ThreadID = "another-conversation" }},
		{"message", func(_ *bridgeConfig, i *invocation) { i.Message += " Different intent." }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed, changedIn := cfg, in
			tc.edit(&changed, &changedIn)
			if _, err := loadOrSaveReceipt(changed, changed.URL, changedIn); err == nil {
				t.Fatal("a changed binding reused the saved operation")
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, original) {
				t.Fatal("conflict replaced the original receipt")
			}
		})
	}
	// Credential rotation must not make the caller choose a new operation ID.
	cfg.Key = "rotated-synthetic-credential"
	second, err := loadOrSaveReceipt(cfg, cfg.URL, in)
	if err != nil || *first != *second {
		t.Fatalf("unchanged intent was not replayable after rotation: %v", err)
	}
	other := in
	other.OperationID = "operation-2"
	third, err := loadOrSaveReceipt(cfg, cfg.URL, other)
	if err != nil || third.RunID == first.RunID {
		t.Fatalf("identical text for a new turn did not get distinct work: %v", err)
	}
}

func TestReceiptPrivateIntentOnly(t *testing.T) {
	cfg := receiptTestConfig(t)
	in := receiptTestInvocation()
	r, err := loadOrSaveReceipt(cfg, cfg.URL, in)
	if err != nil {
		t.Fatal(err)
	}
	if first, err := claimAttempt(cfg.State, r); err != nil || !first {
		t.Fatalf("first attempt: first=%v error=%v", first, err)
	}
	if first, err := claimAttempt(cfg.State, r); err != nil || first {
		t.Fatalf("second attempt: first=%v error=%v", first, err)
	}
	entries, err := os.ReadDir(cfg.State)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("unexpected intent store contents: %d entries", len(entries))
	}
	for _, entry := range entries {
		path := filepath.Join(cfg.State, entry.Name())
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			t.Fatalf("intent file is not private: %s error=%v", entry.Name(), err)
		}
		data, err := os.ReadFile(path)
		if err != nil || bytes.Contains(data, []byte(cfg.Key)) || bytes.Contains(data, []byte("Bearer ")) {
			t.Fatal("credential persisted in intent store")
		}
		if strings.HasSuffix(entry.Name(), ".json") {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"version", "endpoint", "profile", "agent_id", "run_id", "operation_id", "thread_id", "message"} {
				if _, ok := fields[name]; !ok {
					t.Fatalf("missing intent field %s", name)
				}
				delete(fields, name)
			}
			if len(fields) != 0 {
				t.Fatalf("unexpected persisted fields, such as status/output: %v", fields)
			}
		}
	}
}

func TestReceiptRejectsUnsafeState(t *testing.T) {
	t.Run("relative_directory", func(t *testing.T) {
		if err := prepareDirectory("relative-state"); err == nil {
			t.Fatal("relative state accepted")
		}
	})
	t.Run("public_directory", func(t *testing.T) {
		directory := t.TempDir()
		if err := os.Chmod(directory, 0755); err != nil {
			t.Fatal(err)
		}
		if err := prepareDirectory(directory); err == nil {
			t.Fatal("nonprivate state accepted")
		}
	})
	t.Run("symlink_directory", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "state")
		if err := os.Symlink(t.TempDir(), link); err != nil {
			t.Fatal(err)
		}
		if err := prepareDirectory(link); err == nil {
			t.Fatal("symlink state accepted")
		}
	})
	for _, kind := range []string{"symlink", "public_mode", "truncated", "oversized", "directory"} {
		t.Run(kind, func(t *testing.T) {
			cfg := receiptTestConfig(t)
			in := receiptTestInvocation()
			path := filepath.Join(cfg.State, receiptName(in.OperationID)+".json")
			switch kind {
			case "symlink":
				if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			default:
				data := []byte(`{"version":1`)
				mode := os.FileMode(0600)
				if kind == "oversized" {
					data = bytes.Repeat([]byte{'x'}, 65537)
				}
				if kind == "public_mode" {
					mode = 0644
				}
				if err := os.WriteFile(path, data, mode); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, mode); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := loadOrSaveReceipt(cfg, cfg.URL, in); err == nil {
				t.Fatal("unsafe or partial receipt accepted")
			}
		})
	}
}

func TestReceiptAttemptMarkerRejectsSymlinkAndPublicMode(t *testing.T) {
	for _, kind := range []string{"symlink", "public", "directory"} {
		t.Run(kind, func(t *testing.T) {
			cfg := receiptTestConfig(t)
			r, err := loadOrSaveReceipt(cfg, cfg.URL, receiptTestInvocation())
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(cfg.State, receiptName(r.OperationID)+".attempt")
			if kind == "symlink" {
				err = os.Symlink(filepath.Join(t.TempDir(), "elsewhere"), marker)
			} else if kind == "directory" {
				err = os.Mkdir(marker, 0700)
			} else {
				err = os.WriteFile(marker, []byte("attempted\n"), 0644)
				if err == nil {
					err = os.Chmod(marker, 0644)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := claimAttempt(cfg.State, r); err == nil {
				t.Fatal("unsafe attempt marker accepted")
			}
		})
	}
}

func TestReceiptEscapedMessageWithinInvocationBound(t *testing.T) {
	cfg := receiptTestConfig(t)
	in := receiptTestInvocation()
	in.Message = "bounded " + strings.Repeat("\x01", 4000)
	args, err := json.Marshal(in)
	if err != nil || len(args) >= 32768 || len(in.Message) >= 8192 {
		t.Fatal("fixture is outside the advertised invocation bounds")
	}
	r, err := loadOrSaveReceipt(cfg, cfg.URL, in)
	if err != nil {
		t.Fatalf("valid escaped message could not be saved: %v", err)
	}
	if again, err := loadOrSaveReceipt(cfg, cfg.URL, in); err != nil || *again != *r {
		t.Fatalf("valid escaped message could not be reloaded: %v", err)
	}
}

func TestReceiptProcessHelper(t *testing.T) {
	if os.Getenv("RUNKITE_RECEIPT_TEST_CHILD") != "1" {
		return
	}
	cfg := bridgeConfig{URL: "https://specialist.example", Agent: "specialist", Profile: "cross-process", State: os.Getenv("RUNKITE_RECEIPT_TEST_STATE")}
	r, err := loadOrSaveReceipt(cfg, cfg.URL, receiptTestInvocation())
	if err != nil {
		fmt.Fprintln(os.Stderr, "save failed")
		os.Exit(2)
	}
	first, err := claimAttempt(cfg.State, r)
	if err != nil {
		fmt.Fprintln(os.Stderr, "claim failed")
		os.Exit(3)
	}
	_ = json.NewEncoder(os.Stdout).Encode(struct {
		RunID string `json:"run_id"`
		First bool   `json:"first"`
	}{r.RunID, first})
	os.Exit(0)
}

func TestReceiptConcurrentProcessesPublishOneIntentAndAttempt(t *testing.T) {
	state := receiptTestDirectory(t)
	const count = 8
	commands := make([]*exec.Cmd, count)
	outputs := make([]bytes.Buffer, count)
	for i := range commands {
		command := exec.Command(os.Args[0], "-test.run=^TestReceiptProcessHelper$")
		command.Env = append(os.Environ(), "RUNKITE_RECEIPT_TEST_CHILD=1", "RUNKITE_RECEIPT_TEST_STATE="+state)
		command.Stdout, command.Stderr = &outputs[i], &outputs[i]
		commands[i] = command
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
	}
	firstCount, expectedID := 0, ""
	for i, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatalf("child %d: %v, output=%s", i, err, outputs[i].String())
		}
		var result struct {
			RunID string `json:"run_id"`
			First bool   `json:"first"`
		}
		if err := json.Unmarshal(outputs[i].Bytes(), &result); err != nil || result.RunID == "" {
			t.Fatalf("invalid child receipt: %s", outputs[i].String())
		}
		if expectedID == "" {
			expectedID = result.RunID
		}
		if result.RunID != expectedID {
			t.Fatal("processes chose different run identities for one intent")
		}
		if result.First {
			firstCount++
		}
	}
	if firstCount != 1 {
		t.Fatalf("expected one durable first-send claim, got %d", firstCount)
	}
	entries, err := os.ReadDir(state)
	if err != nil || len(entries) != 2 {
		t.Fatalf("partial/temp files remained: entries=%v error=%v", entries, err)
	}
}
