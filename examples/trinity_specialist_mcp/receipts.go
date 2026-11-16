package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Receipts are private caller intent. Runkite remains the authority for status.
type receipt struct {
	Version  int    `json:"version"`
	Endpoint string `json:"endpoint"`
	Profile  string `json:"profile"`
	AgentID  string `json:"agent_id"`
	RunID    string `json:"run_id"`
	invocation
}

func prepareDirectory(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("absolute state directory required")
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("private directory required")
	}
	return nil
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
	dir, err := os.Open(directory)
	if err != nil {
		return false, err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return false, err
	}
	return created, nil
}
