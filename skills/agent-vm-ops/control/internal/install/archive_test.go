package install

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func archive(name string, kind byte, size int64) *bytes.Buffer {
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	_ = w.WriteHeader(&tar.Header{Name: name, Typeflag: kind, Mode: 0644, Size: size, Linkname: "/etc/passwd"})
	if kind == tar.TypeReg {
		_, _ = w.Write(bytes.Repeat([]byte("x"), int(size)))
	}
	_ = w.Close()
	return &b
}
func TestExtractRejectsEscapeAndLinks(t *testing.T) {
	for _, test := range []struct {
		name string
		kind byte
	}{{"../escape", tar.TypeReg}, {"/absolute", tar.TypeReg}, {"a/../escape", tar.TypeReg}, {"link", tar.TypeSymlink}, {"hardlink", tar.TypeLink}, {"fifo", tar.TypeFifo}} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			destination := filepath.Join(root, "stage")
			if err := Extract(archive(test.name, test.kind, 0), destination, 1024); err == nil {
				t.Fatal("unsafe member accepted")
			}
			if _, err := os.Lstat(destination); !os.IsNotExist(err) {
				t.Fatal("failed staging tree retained")
			}
		})
	}
}
func TestExtractBudgetAndExistingTree(t *testing.T) {
	root := t.TempDir()
	destination := filepath.Join(root, "stage")
	if err := Extract(archive("file", tar.TypeReg, 3), destination, 2); err == nil {
		t.Fatal("budget ignored")
	}
	if err := Extract(archive("file", tar.TypeReg, 3), destination, 3); err != nil {
		t.Fatal(err)
	}
	if err := Extract(archive("other", tar.TypeReg, 0), destination, 3); err == nil {
		t.Fatal("existing tree overwritten")
	}
	data, err := os.ReadFile(filepath.Join(destination, "file"))
	if err != nil || string(data) != "xxx" {
		t.Fatal("existing tree changed")
	}
}
