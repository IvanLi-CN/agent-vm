package manifest

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ReadProductArchive verifies an archive entirely through reads, so installer
// preflight creates neither staging files nor persistent installation records.
func ReadProductArchive(source io.Reader) (Metadata, error) {
	type entry struct {
		mode   int64
		digest string
		header []byte
	}
	entries := map[string]entry{}
	reader := tar.NewReader(io.LimitReader(source, 1<<30+1))
	var manifestBytes []byte
	var total int64
	for {
		h, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Metadata{}, err
		}
		if !safeRelativePath(h.Name) || (h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA) || h.Mode & ^int64(0777) != 0 || h.Size < 0 || h.Size > 256<<20 || h.Size > (1<<30)-total || len(entries) >= 10000 {
			return Metadata{}, errors.New("unsafe or oversized product archive")
		}
		if _, ok := entries[h.Name]; ok {
			return Metadata{}, errors.New("duplicate archive member")
		}
		total += h.Size
		hash := sha256.New()
		first := make([]byte, min(h.Size, 20))
		if _, err := io.ReadFull(reader, first); err != nil {
			return Metadata{}, err
		}
		hash.Write(first)
		var data bytes.Buffer
		var writer io.Writer = hash
		if h.Name == "release-manifest.json" {
			if h.Size > maxBytes {
				return Metadata{}, errors.New("manifest too large")
			}
			data.Write(first)
			writer = io.MultiWriter(hash, &data)
		}
		if _, err := io.Copy(writer, reader); err != nil {
			return Metadata{}, err
		}
		if h.Name == "release-manifest.json" {
			manifestBytes = data.Bytes()
		}
		entries[h.Name] = entry{h.Mode, hex.EncodeToString(hash.Sum(nil)), first}
	}
	var root map[string]any
	decoder := json.NewDecoder(bytes.NewReader(manifestBytes))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return Metadata{}, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Metadata{}, errors.New("trailing manifest data")
	}
	if err := validateProductUsing(root, func(artifacts []checkedArtifact) error {
		allowed := map[string]bool{"release-manifest.json": true, "client-release.json": true}
		for _, a := range artifacts {
			allowed[a.path] = true
			e, ok := entries[a.path]
			if !ok || e.mode != a.mode || e.digest != a.sha256 {
				return fmt.Errorf("archive artifact mismatch: %s", a.path)
			}
			if isGoBinaryArtifact(a.path) {
				if err := verifyELFHeader(e.header); err != nil {
					return err
				}
			}
		}
		for name := range entries {
			if !allowed[name] {
				return fmt.Errorf("undeclared archive member: %s", name)
			}
		}
		return nil
	}); err != nil {
		return Metadata{}, err
	}
	result, err := metadataFromRoot(root)
	if err != nil {
		return Metadata{}, err
	}
	result.ManifestPresent = true
	result.ArtifactsVerified = true
	return result, nil
}
