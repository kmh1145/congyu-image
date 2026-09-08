package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestTypeAllowed(t *testing.T) {
	if !typeAllowed("image/jpeg", "image/jpeg,image/png") {
		t.Fatal("jpeg should be allowed")
	}
	if typeAllowed("image/gif", "image/jpeg,image/png") {
		t.Fatal("gif should be rejected")
	}
}

func TestSanitizeFilename(t *testing.T) {
	if got := sanitizeFilename("../../hello.jpg"); got != "hello.jpg" {
		t.Fatalf("unexpected filename %q", got)
	}
}

func TestLocalStoreRoundTrip(t *testing.T) {
	root := t.TempDir()
	store := &localStore{root: root}
	payload := []byte("congyu")
	if err := store.Put(context.Background(), "2026/09/test.webp", bytes.NewReader(payload), int64(len(payload)), "image/webp"); err != nil {
		t.Fatal(err)
	}
	r, err := store.Open(context.Background(), "2026/09/test.webp")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if !bytes.Equal(got, payload) {
		t.Fatalf("got %q", got)
	}
	if err = store.Delete(context.Background(), "2026/09/test.webp"); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(root, "2026/09/test.webp")); !os.IsNotExist(err) {
		t.Fatal("object was not deleted")
	}
}

func TestLocalStoreRejectsTraversal(t *testing.T) {
	store := &localStore{root: t.TempDir()}
	if err := store.Put(context.Background(), "../escape", bytes.NewReader(nil), 0, ""); err == nil {
		t.Fatal("expected traversal rejection")
	}
}

func TestSettingsValidators(t *testing.T) {
	if !validHexColor("#3b82f6") || validHexColor("blue") {
		t.Fatal("color validation failed")
	}
	if !validTypeList("image/jpeg,image/png") || validTypeList("text/html") {
		t.Fatal("mime validation failed")
	}
}
