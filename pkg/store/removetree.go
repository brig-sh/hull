// Copyright (c) 2026, NOFire AI
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package store

import (
	"errors"
	"io/fs"
	"os"
)

// RemoveTree is os.RemoveAll for a tree that an image unpack wrote.
//
// An image is free to ship directories its owner may not write into: Fedora
// has dozens of 0555 directories under /usr/lib, and the unpack puts the
// image's mode on the host directory. Unlinking an entry needs write
// permission on the directory that holds it, so a plain RemoveAll stops with
// EACCES on the first one, and `hull rmi`, prune and the pull's own sweeps
// leave the image behind -- as the user who owns every file in it.
//
// The plain removal runs first because it is what nearly every tree needs,
// and only a permission failure pays for the second walk.
func RemoveTree(path string) error {
	err := os.RemoveAll(path)
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		return err
	}
	if openErr := openTreeForRemoval(path); openErr != nil {
		return err
	}
	return os.RemoveAll(path)
}

// openTreeForRemoval adds the owner's rwx to every directory under path.
//
// The walk goes through an os.Root, as every other walk of an image tree in
// hull does: the tree is the image's, a directory in it may be an absolute
// symlink, and a chmod that followed one would loosen a host directory the
// image chose. fs.WalkDir calls back on a directory before it reads it, so
// even a 0000 directory is opened up in time to be listed.
//
// Failures on single entries are not reported here. Whatever could not be
// opened up makes the second RemoveAll fail, and that error names the entry.
func openTreeForRemoval(path string) error {
	root, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil || info.Mode().Perm()&0o700 == 0o700 {
			return nil
		}
		_ = root.Chmod(name, info.Mode().Perm()|0o700)
		return nil
	})
}
