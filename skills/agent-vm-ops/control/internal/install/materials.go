package install

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

type Material struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Mode   uint32 `json:"mode"`
}
type MaterialsManifest struct {
	SchemaVersion  int        `json:"schema_version"`
	SourceRevision string     `json:"source_revision"`
	Files          []Material `json:"files"`
}

// ValidateMaterials performs read-only verification before any installer action.
// Link-free traversal is required for every path component, including the root.
func ValidateMaterials(root, revision string) (MaterialsManifest, error) {
	var result MaterialsManifest
	if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(revision) {
		return result, errors.New("invalid source revision")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return result, err
	}
	file, err := materialFile(abs, "materials-manifest.json")
	if err != nil {
		return result, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 4<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return result, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return result, errors.New("trailing materials manifest data")
	}
	if result.SchemaVersion != 1 || result.SourceRevision != revision || len(result.Files) == 0 || len(result.Files) > 10000 {
		return result, errors.New("materials schema, source revision, or file count mismatch")
	}
	seen := map[string]bool{}
	for _, entry := range result.Files {
		if seen[entry.Path] || entry.Path == "materials-manifest.json" {
			return result, errors.New("duplicate or reserved material path")
		}
		seen[entry.Path] = true
		if entry.Mode&^0777 != 0 || entry.Mode&0022 != 0 {
			return result, errors.New("unsafe materials permissions")
		}
		digest, err := hex.DecodeString(entry.SHA256)
		if err != nil || len(digest) != sha256.Size {
			return result, errors.New("invalid materials digest")
		}
		source, err := materialFile(abs, entry.Path)
		if err != nil {
			return result, err
		}
		info, err := source.Stat()
		if err != nil {
			source.Close()
			return result, err
		}
		hash := sha256.New()
		n, err := io.Copy(hash, io.LimitReader(source, (256<<20)+1))
		source.Close()
		if err != nil {
			return result, err
		}
		if n > 256<<20 {
			return result, errors.New("material exceeds size limit")
		}
		if info.Mode().Perm() != os.FileMode(entry.Mode) || hex.EncodeToString(hash.Sum(nil)) != entry.SHA256 {
			return result, fmt.Errorf("material integrity mismatch: %s", entry.Path)
		}
	}
	err = filepath.WalkDir(abs, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("unlisted symlink in materials")
		}
		if entry.IsDir() {
			return nil
		}
		name, err := filepath.Rel(abs, current)
		if err != nil {
			return err
		}
		if name != "materials-manifest.json" && !seen[name] {
			return fmt.Errorf("unlisted deployment material: %s", name)
		}
		return nil
	})
	if err != nil {
		return result, err
	}
	return result, nil
}
func materialFile(root, name string) (*os.File, error) {
	if name == "" || filepath.IsAbs(name) || strings.Contains(name, "\\") || filepath.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") {
		return nil, errors.New("unsafe material path")
	}
	full := filepath.Join(root, name)
	for current := full; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return nil, err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || (int(st.Uid) != 0 && int(st.Uid) != os.Geteuid()) {
			return nil, errors.New("material path is not owned by the invoking user or root")
		}
		if info.Mode().Perm()&0022 != 0 && !(current != full && info.IsDir() && info.Mode()&os.ModeSticky != 0 && st.Uid == 0) {
			return nil, errors.New("material path is writable by other users")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("material path traverses a symlink")
		}
		if current == full && !info.Mode().IsRegular() {
			return nil, errors.New("material is not a regular file")
		}
		if current != full && !info.IsDir() {
			return nil, errors.New("material parent is not a directory")
		}
		if current == filepath.Dir(current) {
			break
		}
	}
	return os.Open(full)
}
