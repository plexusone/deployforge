package onhost

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/plexusone/deployforge/deploy/secrets"
)

// etcFiles are the files a release installs under /etc/<unit>: config.yaml, env,
// .managed (the key files the last deploy installed) and the key files. Every
// name this deploy or the last one touches is saved first, so a failure puts
// back exactly what was there.
type etcFiles struct {
	h     *Host
	dir   string
	group string
	// old are the key files the previous deploy installed.
	old []string
	// names are every file this deploy may change.
	names []string
}

func (e *etcFiles) at(name string) string { return filepath.Join(e.dir, name) }

// save keeps each existing file as <name>.prev, as a hard link, so its owner,
// mode and contents survive the atomic replacements install makes.
func (e *etcFiles) save() error {
	for _, n := range e.names {
		prev := e.at(n) + ".prev"
		if err := os.Remove(prev); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("onhost: %w", err)
		}
		if err := os.Link(e.at(n), prev); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("onhost: save %s: %w", n, err)
		}
	}
	return nil
}

// install writes this deploy's files, removes key files the secret store no
// longer has, and records the key files it installed.
func (e *etcFiles) install(config, env []byte, files []secrets.File) error {
	if err := e.write("config.yaml", config); err != nil {
		return err
	}
	if err := e.write("env", env); err != nil {
		return err
	}
	names := make([]string, 0, len(files))
	for _, f := range files {
		if err := e.write(f.Name, f.Data); err != nil {
			return err
		}
		names = append(names, f.Name)
	}
	for _, n := range e.old {
		if !slices.Contains(names, n) {
			if err := os.Remove(e.at(n)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("onhost: %w", err)
			}
		}
	}
	managed := ""
	if len(names) > 0 {
		managed = strings.Join(names, "\n") + "\n"
	}
	return e.write(".managed", []byte(managed))
}

// write replaces a file atomically, root:<group> 0640.
func (e *etcFiles) write(name string, data []byte) error {
	tmp, err := os.CreateTemp(e.dir, "."+name+".new-*")
	if err != nil {
		return fmt.Errorf("onhost: %w", err)
	}
	// fail reports err, and a failure to remove the temporary file with it,
	// since a temporary file left behind could hold a secret.
	fail := func(err error, closeFirst bool) error {
		if closeFirst {
			// Already failing: a close error adds nothing to the cause.
			if cerr := tmp.Close(); cerr != nil && !errors.Is(cerr, os.ErrClosed) {
				err = errors.Join(err, cerr)
			}
		}
		if rerr := os.Remove(tmp.Name()); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("removing %s: %w", tmp.Name(), rerr))
		}
		return fmt.Errorf("onhost: write %s: %w", name, err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err, true)
	}
	if err := tmp.Chmod(0o640); err != nil {
		return fail(err, true)
	}
	if err := tmp.Close(); err != nil {
		return fail(err, false)
	}
	if err := e.h.Sys.Own(tmp.Name(), e.group); err != nil {
		return fail(err, false)
	}
	if err := os.Rename(tmp.Name(), e.at(name)); err != nil {
		return fail(err, false)
	}
	return nil
}

// restore puts back every saved file, and removes a key file this deploy
// installed where there was none before. config.yaml and env always existed
// on a deployed stack, so they are left as they are when there was none.
func (e *etcFiles) restore() error {
	var errs []error
	for _, n := range e.names {
		prev := e.at(n) + ".prev"
		err := os.Rename(prev, e.at(n))
		switch {
		case err == nil:
		case !errors.Is(err, os.ErrNotExist):
			errs = append(errs, err)
		case n == "config.yaml" || n == "env" || slices.Contains(e.old, n):
		default:
			if err := os.Remove(e.at(n)); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("onhost: restoring /etc files: %w", errors.Join(errs...))
	}
	return nil
}

// drop removes the saved files once the new release is live.
func (e *etcFiles) drop() error {
	var errs []error
	for _, n := range e.names {
		if err := os.Remove(e.at(n) + ".prev"); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
