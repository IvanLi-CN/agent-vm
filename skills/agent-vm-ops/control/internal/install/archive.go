// Package install owns local deployment bootstrap validation, not VM lifecycle.
package install

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Extract accepts only regular files and directories in a private, new tree.
// The caller removes the staging tree on any error; no installed path is touched.
func Extract(source io.Reader, destination string, maximum int64) (err error) {
	if maximum <= 0 {
		return errors.New("archive budget must be positive")
	}
	if err = os.Mkdir(destination, 0700); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(destination)
		}
	}()
	reader := tar.NewReader(source)
	seen := map[string]bool{}
	var total int64
	for {
		header, nextErr := reader.Next()
		if nextErr == io.EOF {
			return nil
		}
		if nextErr != nil {
			return nextErr
		}
		name := strings.TrimSuffix(header.Name, "/")
		if name == "" || name == "." {
			if header.Typeflag == tar.TypeDir {
				continue
			}
			return errors.New("invalid archive root")
		}
		if name != path.Clean(name) || path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") || strings.Contains(name, "\\") {
			return fmt.Errorf("unsafe archive path %q", name)
		}
		if seen[name] {
			return fmt.Errorf("duplicate archive member %q", name)
		}
		seen[name] = true
		target := filepath.Join(destination, filepath.FromSlash(name))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0700); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if header.Size < 0 || header.Size > maximum-total {
				return errors.New("archive exceeds size budget")
			}
			total += header.Size
			if header.Mode&07000 != 0 {
				return errors.New("archive contains privileged file mode")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(file, reader, header.Size)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			if err := os.Chmod(target, os.FileMode(header.Mode)&0777); err != nil {
				return err
			}
		default:
			return errors.New("archive links and special files are forbidden")
		}
	}
}

// ExtractGzip applies the same uncompressed budget to dependency archives.
func ExtractGzip(source io.Reader, destination string, maximum int64) error {
	reader, err := gzip.NewReader(source)
	if err != nil {
		return err
	}
	defer reader.Close()
	return Extract(reader, destination, maximum)
}
