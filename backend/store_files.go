package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// Restrict session IDs before using them in any file path. Message IDs use
// <session>.<random>, making UpdateReply routing survive a process restart.
func validSessionID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, ch := range id {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-') {
			return false
		}
	}
	return true
}

func storeID(prefix string) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(random[:]), nil
}

func storeDirectory(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("%w: data directory is empty", ErrStoreInvalid)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0700); err != nil {
		return "", err
	}
	return abs, nil
}

// All file mutations use a same-directory temporary file, fsync and rename.
// Cancellation/errors before rename leave both the old file and memory intact.
// This is a single-process store, not a cross-file or cross-process transaction.
func atomicStoreFile(ctx context.Context, path string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".minicoco-store-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err = file.Write(data); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func validUsage(u Usage) bool {
	return u.Prompt >= 0 && u.Completion >= 0 && u.Total >= 0
}
