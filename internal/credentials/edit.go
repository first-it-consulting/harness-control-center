//go:build unix

package credentials

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"go.yaml.in/yaml/v3"
)

const storeHeader = `# The credential broker's store (internal/credentials): which secret goes
# to which hosts, in which header. The secrets are in the files named here.
# Edited by the Control Center ("hcc credentials"); hand edits are kept as
# long as they are valid, comments are not.
`

// Set adds or replaces the credential e with secret, which it writes to
// a new file in secretsDir. With secret nil, an existing credential keeps
// its secret (changing only where it goes). Files get the store file's
// mode and group, so on a server they stay root's and readable by the
// runner's group only. The store switches to the new entry in one
// rename, and only if it is valid with it; the old secret file goes
// afterwards.
func Set(path, secretsDir string, e Entry, secret []byte) error {
	if !nameRe.MatchString(e.Name) {
		return fmt.Errorf("credentials: invalid name %q", e.Name)
	}
	mode, gid, err := storeMode(path)
	if err != nil {
		return err
	}
	entries, err := readEntries(path)
	if err != nil {
		return err
	}
	old, existed := entries[e.Name]
	fe := fileEntry{Hosts: e.Hosts, Scheme: e.Scheme, Port: e.Port, Header: e.Header, Prefix: e.Prefix}
	switch {
	case secret != nil:
		s := strings.TrimSpace(string(secret))
		if s == "" || len(secret) > MaxSecretBytes || strings.ContainsAny(s, "\r\n\x00") {
			return fmt.Errorf("credentials: %s: the secret must be one line of at most %d bytes", e.Name, MaxSecretBytes)
		}
		if err := secretsDirFor(secretsDir, mode, gid); err != nil {
			return err
		}
		var suffix [6]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return err
		}
		fe.SecretFile = filepath.Join(filepath.Clean(secretsDir), e.Name+"."+hex.EncodeToString(suffix[:]))
		if err := writeOwned(fe.SecretFile, []byte(s+"\n"), mode, gid); err != nil {
			return err
		}
	case existed:
		fe.SecretFile = old.SecretFile
	default:
		return fmt.Errorf("credentials: %s is new, so it needs a secret", e.Name)
	}
	keep := false
	defer func() {
		if !keep && secret != nil {
			_ = os.Remove(fe.SecretFile)
		}
	}()
	if _, err := build(e.Name, fe); err != nil {
		return err
	}
	entries[e.Name] = fe
	data, err := marshalEntries(entries)
	if err != nil {
		return err
	}
	if err := writeOwned(path, data, mode, gid); err != nil {
		return err
	}
	keep = true
	if existed && old.SecretFile != fe.SecretFile {
		removeSecret(old.SecretFile, secretsDir)
	}
	return nil
}

// secretsDirFor creates the secrets directory if needed, searchable by
// whoever may read the secrets (the store's group, on a server).
func secretsDirFor(dir string, mode os.FileMode, gid int) error {
	if _, err := os.Stat(dir); err == nil {
		return nil
	}
	d := os.FileMode(0o700)
	if mode&0o040 != 0 {
		d |= 0o050
	}
	if err := os.Mkdir(dir, d); err != nil {
		return err
	}
	return os.Lchown(dir, -1, gid)
}

// removeSecret deletes a secret file the store no longer names, if it is
// one of ours (a hand-written entry may name a file elsewhere, which is
// left alone).
func removeSecret(file, secretsDir string) {
	if filepath.Dir(file) == filepath.Clean(secretsDir) {
		_ = os.Remove(file)
	}
}

// Remove deletes a credential and its secret file (see removeSecret).
func Remove(path, secretsDir, name string) error {
	mode, gid, err := storeMode(path)
	if err != nil {
		return err
	}
	entries, err := readEntries(path)
	if err != nil {
		return err
	}
	e, ok := entries[name]
	if !ok {
		return fmt.Errorf("credentials: no credential called %q", name)
	}
	delete(entries, name)
	data, err := marshalEntries(entries)
	if err != nil {
		return err
	}
	if err := writeOwned(path, data, mode, gid); err != nil {
		return err
	}
	removeSecret(e.SecretFile, secretsDir)
	return nil
}

func marshalEntries(entries map[string]fileEntry) ([]byte, error) {
	if len(entries) == 0 {
		return []byte(storeHeader + "{}\n"), nil
	}
	data, err := yaml.Marshal(entries)
	if err != nil {
		return nil, err
	}
	return append([]byte(storeHeader), data...), nil
}

// storeMode is the store file's mode and group, which new files copy.
// It must already be private (the runner refuses anything else).
func storeMode(path string) (os.FileMode, int, error) {
	if err := checkPrivate(path); err != nil {
		return 0, 0, fmt.Errorf("credentials: %w", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("credentials: cannot read the group of %s", path)
	}
	return fi.Mode().Perm(), int(st.Gid), nil
}

// writeOwned replaces path atomically with data, mode and group.
func writeOwned(path string, data []byte, mode os.FileMode, gid int) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, werr := tmp.Write(data)
	if err := errors.Join(werr, tmp.Chmod(mode), tmp.Sync(), tmp.Close()); err != nil {
		return err
	}
	if err := os.Lchown(tmp.Name(), -1, gid); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
