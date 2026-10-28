package webassets

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
)

type Entry struct {
	Path   string
	SHA256 string
	Size   int64
}

func VerifyFS(files fs.FS, entries []Entry) error {
	if files == nil || len(entries) == 0 {
		return errors.New("web_assets_missing: embedded Web assets are unavailable")
	}
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry.Path == "" || entry.Path[0] == '/' || entry.Path == "." {
			return fmt.Errorf("web_assets_manifest_invalid: asset path %q is invalid", entry.Path)
		}
		if _, ok := seen[entry.Path]; ok {
			return fmt.Errorf("web_assets_manifest_invalid: duplicate asset %q", entry.Path)
		}
		seen[entry.Path] = struct{}{}
		data, err := fs.ReadFile(files, entry.Path)
		if err != nil {
			return fmt.Errorf("web_assets_missing: %s: %w", entry.Path, err)
		}
		if int64(len(data)) != entry.Size {
			return fmt.Errorf("web_assets_size_mismatch: %s", entry.Path)
		}
		digest := sha256.Sum256(data)
		if hex.EncodeToString(digest[:]) != entry.SHA256 {
			return fmt.Errorf("web_assets_checksum_mismatch: %s", entry.Path)
		}
	}
	if _, err := fs.Stat(files, "index.html"); err != nil {
		return fmt.Errorf("web_assets_missing: index.html: %w", err)
	}
	return nil
}
