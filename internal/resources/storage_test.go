package resources

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestStorageValidatedPrivateFiles(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, DefaultQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	text := []byte("Lecture notes\nUnicode: λ, ẹ\n")
	file, err := s.Save(bytes.NewReader(text), `C:\Users\Student\..\notes.txt`)
	if err != nil || file.OriginalName != "notes.txt" || file.MIME != "text/plain" || file.Size != int64(len(text)) || !validKey(file.Key) {
		t.Fatalf("file=%+v err=%v", file, err)
	}
	opened, err := s.Open(file.Key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(opened)
	opened.Close()
	if err != nil || !bytes.Equal(got, text) {
		t.Fatalf("data=%q err=%v", got, err)
	}
	if err := s.Remove(file.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(file.Key); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed file open: %v", err)
	}
	if err := s.Remove(file.Key); err != nil {
		t.Fatalf("repeat removal: %v", err)
	}
	pdf, err := s.Save(strings.NewReader("%PDF-1.7\n1 0 obj\n<<>>\nendobj\n%%EOF\n"), "notes.PDF")
	if err != nil || pdf.MIME != "application/pdf" {
		t.Fatalf("PDF=%+v err=%v", pdf, err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 || entries[0].Name() != pdf.Key {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
}

func TestStorageRejectsInvalidContentAndPaths(t *testing.T) {
	s, err := New(t.TempDir(), DefaultQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, data string }{
		{"empty.txt", ""}, {"notes.pdf", "Plain text"}, {"notes.txt", "%PDF-1.7\n%%EOF"},
		{"notes.html", "Plain text"}, {"notes.txt", "<html><body>Hello</body></html>"},
		{"notes.txt", "\ufeff \n<script>alert(1)</script>"}, {"notes.txt", "prefix\n<svg onload='alert(1)'>"},
		{"notes.txt", "binary\x00content"}, {"notes.txt", string([]byte{0xff, 0xfe})}, {"truncated.pdf", "%PDF-1.7\nno eof"},
		{"../../", "text"}, {strings.Repeat("x", 181) + ".txt", "text"},
	} {
		t.Run(tc.name+tc.data[:min(len(tc.data), 5)], func(t *testing.T) {
			if _, err := s.Save(strings.NewReader(tc.data), tc.name); !errors.Is(err, ErrUnsupportedType) {
				t.Fatalf("name=%q error=%v", tc.name, err)
			}
		})
	}
	for _, key := range []string{"../secret", "", strings.Repeat("A", 64), strings.Repeat("0", 63), strings.Repeat("0", 64) + "/../secret"} {
		if _, err := s.Open(key); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unsafe open %q: %v", key, err)
		}
		if err := s.Remove(key); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unsafe remove %q: %v", key, err)
		}
	}
	if _, err := s.Save(io.MultiReader(strings.NewReader("x"), alwaysErrorReader{}), "error.txt"); err == nil {
		t.Fatal("accepted failing reader")
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("rejections retained files: %v %v", entries, err)
	}
}

type alwaysErrorReader struct{}

func (alwaysErrorReader) Read([]byte) (int, error) { return 0, errors.New("reader failed") }

func TestStorageLimitsAndConcurrentQuota(t *testing.T) {
	s, err := New(t.TempDir(), DefaultQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(strings.NewReader(strings.Repeat("x", int(MaxFileBytes)+1)), "large.txt"); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("large error=%v", err)
	}
	if _, err := s.Save(strings.NewReader(strings.Repeat("x", int(MaxFileBytes))), "maximum.txt"); err != nil {
		t.Fatalf("maximum file rejected: %v", err)
	}
	dir := t.TempDir()
	s, err = New(dir, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "orphan.part"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.Save(strings.NewReader("data"), "notes.txt"); results <- err }()
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrQuota) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("quota successes=%d want 1", successes)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if used, err := diskUsage(root); err != nil || used != 7 {
		t.Fatalf("quota usage=%d err=%v", used, err)
	}
}

func TestStorageRejectsSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, DefaultQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("a", 64)
	if err := os.Symlink(outside, filepath.Join(dir, key)); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	if _, err := s.Open(key); err == nil {
		t.Fatal("symlink exposed outside content")
	}
	if _, err := s.Save(strings.NewReader("hello"), "notes.txt"); err == nil {
		t.Fatal("storage ignored unexpected symlink")
	}
}

func TestStorageCountsTinyFilesAndOrphans(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, DefaultQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxStoredFiles-1; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("orphan-%04d.part", i)), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	results := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.Save(strings.NewReader("x"), "tiny.txt"); results <- err }()
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrQuota) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("tiny-file concurrent successes=%d want1", successes)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != MaxStoredFiles {
		t.Fatalf("stored count=%d error=%v", len(entries), err)
	}
}
