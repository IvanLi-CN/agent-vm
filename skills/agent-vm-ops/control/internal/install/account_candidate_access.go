package install

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/config"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd"
)

func prepareCandidateAccess(ctx context.Context, slot string, input Inputs) error {
	if slot != "blue" && slot != "green" {
		return errors.New("invalid account migration candidate")
	}
	cfg, err := LoadConfig(input.Config)
	if err != nil {
		return err
	}
	name := slot + "_config"
	runtimeConfig, err := config.Load(cfg.Files[name])
	if err != nil || runtimeConfig.Slot != slot {
		return errors.New("invalid account migration candidate configuration")
	}
	targetFile := targets[name].path
	expected, err := matchingCandidateConfig(targetFile, cfg.Files[name])
	if err != nil {
		return err
	}
	assets := filepath.Join(input.Materials, "assets")
	const accountsFile = "/etc/sysusers.d/agent-vm-server.conf"
	if err := copyInstall(filepath.Join(assets, "sysusers.d/agent-vm-server.conf"), accountsFile, 0644, ""); err != nil {
		return err
	}
	if err := command(ctx, "/usr/bin/systemd-sysusers", accountsFile); err != nil {
		return err
	}
	// Filter tmpfiles rules so the incumbent slot and the deployment writer
	// retain their original directory ownership throughout candidate startup.
	for _, rule := range []struct {
		file, prefix string
	}{
		{"agent-vm-native.conf", filepath.Dir(targetFile)},
		{"agent-vm-native.conf", "/etc/agent-vm-native/incus"},
		{"agent-vm-fresh-accounts.conf", deployd.ServerRuntimeHome},
		{"agent-vm-fresh-accounts.conf", deployd.ServerLockDir},
	} {
		if err := command(ctx, "/usr/bin/systemd-tmpfiles", "--create", "--prefix="+rule.prefix, filepath.Join(assets, "tmpfiles.d", rule.file)); err != nil {
			return err
		}
	}
	for _, entry := range []string{"incus_client_certificate", "incus_client_private_key", "incus_server_certificate"} {
		item := targets[entry]
		data, err := protectedRead(cfg.Files[entry])
		if err != nil {
			return err
		}
		if err := installFile(item.path, data, item.mode, item.group); err != nil {
			return err
		}
	}
	incus, err := incusCLIConfig(runtimeConfig.Project)
	if err != nil {
		return err
	}
	if err := installFile("/etc/agent-vm-native/incus/config.yml", incus, 0640, "agent-vm-server"); err != nil {
		return err
	}
	ca, err := protectedRead(cfg.Files["rqlite_certificate"])
	if err != nil {
		return err
	}
	if err := installFile(deployd.ServerAuthorityCA, ca, 0640, "agent-vm-server"); err != nil {
		return err
	}
	group, err := user.LookupGroup("agent-vm-server")
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(group.Gid)
	if err != nil || gid <= 0 {
		return errors.New("invalid server group for account migration")
	}
	return grantCandidateConfigRead(targetFile, expected, gid)
}

func matchingCandidateConfig(installedPath, inputPath string) ([]byte, error) {
	expected, err := protectedRead(inputPath)
	if err != nil {
		return nil, err
	}
	installed, err := protectedRead(installedPath)
	if err != nil || !bytes.Equal(expected, installed) {
		return nil, errors.New("account migration preserves the installed candidate configuration; use its exact bytes as input")
	}
	return expected, nil
}

func grantCandidateConfigRead(path string, expected []byte, gid int) error {
	file, err := materialFile(filepath.Dir(path), filepath.Base(path))
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || stat.Nlink != 1 || info.Mode().Perm()&0027 != 0 || info.Size() != int64(len(expected)) {
		return errors.New("candidate configuration is not a protected root-owned file")
	}
	content, err := io.ReadAll(io.LimitReader(file, int64(len(expected))+1))
	if err != nil || !bytes.Equal(content, expected) {
		return errors.New("candidate configuration changed during preparation")
	}
	if err := file.Chown(0, gid); err != nil {
		return err
	}
	if err := file.Chmod(0640); err != nil {
		return err
	}
	return file.Sync()
}
