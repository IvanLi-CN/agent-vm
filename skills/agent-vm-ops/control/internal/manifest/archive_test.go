package manifest

import (
	"archive/tar"
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestReadProductArchiveMatchesDirectoryAndRejectsTampering(t *testing.T) {
	for _, scenario := range []string{"valid", "tampered", "link", "escape", "undeclared"} {
		t.Run(scenario, func(t *testing.T) {
			path := validProductManifestFormat(t, ProductFormat, "go-request")
			root := filepath.Dir(path)
			var archive bytes.Buffer
			writer := tar.NewWriter(&archive)
			err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() {
					return nil
				}
				info, err := d.Info()
				if err != nil {
					return err
				}
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				name, _ := filepath.Rel(root, path)
				if scenario == "tampered" && name == "byte-fs.ts" {
					data = append(data, 'x')
				}
				header := &tar.Header{Name: filepath.ToSlash(name), Typeflag: tar.TypeReg, Mode: int64(info.Mode().Perm()), Size: int64(len(data))}
				if err := writer.WriteHeader(header); err != nil {
					return err
				}
				_, err = writer.Write(data)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "link" {
				err = writer.WriteHeader(&tar.Header{Name: "extra", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"})
			}
			if scenario == "escape" {
				err = writer.WriteHeader(&tar.Header{Name: "../outside", Typeflag: tar.TypeReg, Mode: 0644})
			}
			if scenario == "undeclared" {
				err = writer.WriteHeader(&tar.Header{Name: "extra", Typeflag: tar.TypeReg, Mode: 0644})
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			metadata, err := ReadProductArchive(&archive)
			if scenario != "valid" {
				if err == nil {
					t.Fatal("unsafe archive accepted")
				}
				return
			}
			if err != nil || !metadata.ArtifactsVerified || len(metadata.StateWrites) == 0 {
				t.Fatalf("archive: %+v %v", metadata, err)
			}
		})
	}
}
