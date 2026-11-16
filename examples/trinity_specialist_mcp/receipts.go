package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Receipts are private caller intent. Runkite remains the authority for status.
type receipt struct {
	RunReference string `json:"-"`
	Version      int    `json:"version"`
	Endpoint     string `json:"endpoint"`
	Profile      string `json:"profile"`
	AgentID      string `json:"agent_id"`
	RunID        string `json:"run_id"`
	invocation
}

func prepareDirectory(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("absolute clean state directory required")
	}
	// Require an existing parent so this helper creates only one directory
	// entry, whose durability it can establish before any caller dispatch.
	if err := os.Mkdir(path, 0700); err != nil && !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("private directory required")
	}
	// An existing directory can belong to a concurrent writer whose mkdir
	// has not reached disk yet. Establish our own parent-entry barrier.
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func receiptName(id string) string {
	hash := sha256.Sum256([]byte(id))
	return hex.EncodeToString(hash[:])
}

func loadOrSaveReceipt(cfg bridgeConfig, endpoint string, in invocation) (*receipt, error) {
	if err := prepareDirectory(cfg.State); err != nil {
		return nil, err
	}
	identity, _ := json.Marshal([]string{endpoint, cfg.Profile, cfg.Agent, in.OperationID})
	hash := sha256.Sum256(identity)
	want := receipt{Version: 1, Endpoint: endpoint, Profile: cfg.Profile, AgentID: cfg.Agent,
		RunID: "trinity-" + hex.EncodeToString(hash[:]), invocation: in}
	data, _ := json.Marshal(want)
	if len(data) > 65536 {
		return nil, errors.New("receipt exceeds bound")
	}
	path := filepath.Join(cfg.State, receiptName(in.OperationID)+".json")
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		if _, err := publishPrivateFile(cfg.State, path, data); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 65536 {
		return nil, errors.New("invalid receipt file")
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var got receipt
	if json.Unmarshal(stored, &got) != nil || got != want {
		return nil, errors.New("operation binding changed")
	}
	// Another writer may have linked complete bytes without yet syncing the
	// directory. Do not trust that writer to complete our durability barrier.
	if err := syncDirectory(cfg.State); err != nil {
		return nil, err
	}
	return &got, nil
}

func claimAttempt(directory string, r *receipt) (bool, error) {
	return publishPrivateFile(directory, filepath.Join(directory, receiptName(r.OperationID)+".attempt"), []byte("attempted\n"))
}

// A same-directory hard link publishes a complete, fsynced file without
// replacing a concurrent writer's receipt. Unsupported filesystems fail closed.
func publishPrivateFile(directory, path string, data []byte) (created bool, err error) {
	f, err := os.CreateTemp(directory, ".intent-*")
	if err != nil {
		return false, err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return false, err
	}
	if closeErr != nil {
		return false, closeErr
	}
	if err = os.Link(f.Name(), path); err != nil && !os.IsExist(err) {
		return false, err
	}
	created = err == nil
	if !created {
		info, statErr := os.Lstat(path)
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return false, errors.New("invalid existing operation file")
		}
	}
	if err := syncDirectory(directory); err != nil {
		return false, err
	}
	return created, nil
}

// pinRunReference publishes once, using the same private durable file discipline
// as intent. Conflicting references require reconciliation, never replacement.
func pinRunReference(directory string, r *receipt, reference string) error {
	if len(reference) != len("runkite-run-v1-")+64 || !strings.HasPrefix(reference, "runkite-run-v1-") {
		return errors.New("invalid retained run reference")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(reference, "runkite-run-v1-")); err != nil {
		return errors.New("invalid retained run reference")
	}
	path := filepath.Join(directory, receiptName(r.OperationID)+".run-reference")
	if _, err := publishPrivateFile(directory, path, []byte(reference)); err != nil {
		return err
	}
	if err := loadRunReference(directory, r); err != nil {
		return err
	}
	if r.RunReference != reference {
		return errors.New("retained run reference changed")
	}
	return nil
}

func loadRunReference(directory string, r *receipt) error {
	path := filepath.Join(directory, receiptName(r.OperationID)+".run-reference")
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() != int64(len("runkite-run-v1-")+64) {
		return errors.New("invalid run reference file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	reference := string(raw)
	if !strings.HasPrefix(reference, "runkite-run-v1-") {
		return errors.New("invalid run reference file")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(reference, "runkite-run-v1-")); err != nil {
		return errors.New("invalid run reference file")
	}
	// Reading an in-flight link is not a persistence barrier. Recovery must
	// establish its own barrier before using this immutable identity pin.
	if err := syncDirectory(directory); err != nil {
		return err
	}
	r.RunReference = reference
	return nil
}
