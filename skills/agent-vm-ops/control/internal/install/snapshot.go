package install

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// snapshotInputs copies untrusted inputs into an invoking-user-only tree. No
// copied executable is run until Preflight verifies the complete private tree.
// The returned paths are the sole inputs to every later installer consumer.
func snapshotInputs(input Inputs) (Inputs, func(), error) {
	root, err := os.MkdirTemp("", "agent-vm-install-inputs-")
	if err != nil {
		return Inputs{}, nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return Inputs{}, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(root) }
	fail := func(err error) (Inputs, func(), error) { cleanup(); return Inputs{}, nil, err }
	cfg, err := LoadConfig(input.Config)
	if err != nil {
		return fail(err)
	}
	for name, source := range cfg.Files {
		data, err := protectedRead(source)
		if err != nil {
			return fail(err)
		}
		destination := filepath.Join(root, name)
		if err := os.WriteFile(destination, data, 0600); err != nil {
			return fail(err)
		}
		cfg.Files[name] = destination
	}
	result := Inputs{Config: filepath.Join(root, "install.json"), Bundle: filepath.Join(root, "product.tar"), Materials: filepath.Join(root, "materials")}
	data, err := json.Marshal(cfg)
	if err != nil {
		return fail(err)
	}
	if err := os.WriteFile(result.Config, data, 0600); err != nil {
		return fail(err)
	}
	remaining := int64(1 << 30)
	copyFile := func(source, destination string, preserveMode bool) error {
		f, err := materialFile(filepath.Dir(source), filepath.Base(source))
		if err != nil {
			return err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if info.Size() > remaining {
			return errors.New("installation inputs exceed snapshot budget")
		}
		out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer out.Close()
		n, err := io.Copy(out, io.LimitReader(f, remaining+1))
		if err != nil {
			return err
		}
		remaining -= n
		if remaining < 0 {
			return errors.New("installation inputs exceed snapshot budget")
		}
		if preserveMode {
			return out.Chmod(info.Mode().Perm())
		}
		return nil
	}
	if err := copyFile(input.Bundle, result.Bundle, false); err != nil {
		return fail(err)
	}
	if err := os.Mkdir(result.Materials, 0700); err != nil {
		return fail(err)
	}
	count := 0
	err = filepath.WalkDir(input.Materials, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		count++
		if count > 20000 {
			return errors.New("installation materials exceed entry limit")
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("materials snapshot rejects symlinks")
		}
		name, err := filepath.Rel(input.Materials, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(result.Materials, name)
		if entry.IsDir() {
			if name == "." {
				return nil
			}
			return os.Mkdir(destination, 0700)
		}
		if !entry.Type().IsRegular() {
			return errors.New("materials snapshot rejects special files")
		}
		return copyFile(path, destination, true)
	})
	if err != nil {
		return fail(err)
	}
	return result, cleanup, nil
}
