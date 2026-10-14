// Package resources stores private course files outside the HTTP asset tree.
// One Storage instance owns a directory in the single-process prototype. Multiple
// server replicas require a shared quota mechanism before sharing this directory.
package resources

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

const MaxFileBytes int64 = 5 * 1024 * 1024
const DefaultQuotaBytes int64 = 100 * 1024 * 1024
const MaxStoredFiles = 1000

var (
	ErrTooLarge        = errors.New("resource exceeds 5 MiB")
	ErrUnsupportedType = errors.New("resource must contain PDF or UTF-8 plain text matching its extension")
	ErrQuota           = errors.New("resource storage quota exceeded")
)

type File struct {
	Key, OriginalName, MIME string
	Size                    int64
}

type Storage struct {
	dir   string
	quota int64
	mu    sync.Mutex
}

func New(dir string, quota int64) (*Storage, error) {
	if strings.TrimSpace(dir) == "" || quota <= 0 {
		return nil, errors.New("resource directory and positive quota required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if err := root.Chmod(".", 0700); err != nil {
		return nil, err
	}
	return &Storage{dir: abs, quota: quota}, nil
}

// Save reads at most the file limit plus one byte, validates content, then writes
// a private staged file and renames it atomically. Quota counts actual stored
// bytes, including files orphaned by a crash between saving and the DB commit.
func (s *Storage) Save(reader io.Reader, originalName string) (File, error) {
	data, err := io.ReadAll(io.LimitReader(reader, MaxFileBytes+1))
	if err != nil {
		return File{}, err
	}
	if int64(len(data)) > MaxFileBytes {
		return File{}, ErrTooLarge
	}
	name := cleanName(originalName)
	mimeType, err := contentType(name, data)
	if err != nil {
		return File{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return File{}, err
	}
	defer root.Close()
	used, err := diskUsage(root)
	if err != nil {
		return File{}, err
	}
	if used > s.quota-int64(len(data)) {
		return File{}, ErrQuota
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return File{}, err
	}
	key := hex.EncodeToString(random[:])
	staged := key + ".part"
	file, err := root.OpenFile(staged, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return File{}, err
	}
	committed := false
	defer func() {
		_ = file.Close()
		if !committed {
			_ = root.Remove(staged)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return File{}, err
	}
	if err := file.Sync(); err != nil {
		return File{}, err
	}
	if err := file.Close(); err != nil {
		return File{}, err
	}
	if err := root.Rename(staged, key); err != nil {
		return File{}, err
	}
	committed = true
	return File{Key: key, OriginalName: name, MIME: mimeType, Size: int64(len(data))}, nil
}

func (s *Storage) Open(key string) (*os.File, error) {
	if !validKey(key) {
		return nil, os.ErrNotExist
	}
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	info, err := root.Lstat(key)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, os.ErrNotExist
	}
	return root.Open(key)
}

func (s *Storage) Remove(key string) error {
	if !validKey(key) {
		return os.ErrNotExist
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return err
	}
	defer root.Close()
	err = root.Remove(key)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func validKey(key string) bool {
	if len(key) != 64 {
		return false
	}
	for _, c := range key {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func diskUsage(root *os.Root) (int64, error) {
	dir, err := root.Open(".")
	if err != nil {
		return 0, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(MaxStoredFiles)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	// Bound inode use and quota-scan work even when uploads contain one byte.
	// Orphans and unfinished files consume this count just like committed files.
	if len(entries) >= MaxStoredFiles {
		return 0, ErrQuota
	}
	var used int64
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return 0, err
		}
		// A directory or symlink is unexpected in this flat private store; fail
		// closed rather than skip data that would evade the storage quota.
		if !info.Mode().IsRegular() {
			return 0, fmt.Errorf("unexpected resource storage entry %q", entry.Name())
		}
		if info.Size() > 0 && used > int64(^uint64(0)>>1)-info.Size() {
			return 0, ErrQuota
		}
		used += info.Size()
	}
	return used, nil
}

func cleanName(name string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '\u202e' || r == '\u202d' || r == '\u202a' || r == '\u202b' || r == '\u202c' {
			return -1
		}
		return r
	}, name))
	if name == "." || name == "/" || len(name) > 180 || !utf8.ValidString(name) {
		return ""
	}
	return name
}

func contentType(name string, data []byte) (string, error) {
	if name == "" || len(data) == 0 {
		return "", ErrUnsupportedType
	}
	ext := strings.ToLower(path.Ext(name))
	if ext == ".pdf" {
		if len(data) >= 14 && bytes.HasPrefix(data, []byte("%PDF-")) && http.DetectContentType(data) == "application/pdf" && bytes.HasSuffix(bytes.TrimSpace(data), []byte("%%EOF")) {
			return "application/pdf", nil
		}
		return "", ErrUnsupportedType
	}
	if ext != ".txt" || !utf8.Valid(data) || http.DetectContentType(data) == "application/pdf" {
		return "", ErrUnsupportedType
	}
	for _, r := range string(data) {
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			return "", ErrUnsupportedType
		}
	}
	text := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(string(data), "\ufeff")))
	for _, tag := range []string{"<!doctype", "<html", "<head", "<body", "<script", "<iframe", "<svg", "<object", "<embed", "<style", "<?xml"} {
		if strings.Contains(text, tag) {
			return "", ErrUnsupportedType
		}
	}
	if strings.HasPrefix(http.DetectContentType(data), "text/html") {
		return "", ErrUnsupportedType
	}
	return "text/plain", nil
}
