// Copyright 2022-2026 The Parca Authors
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

//go:build unix

package elfutils

import (
	"fmt"
	"os"
	"syscall"
)

// mmapReadOnly memory-maps the whole file at path read-only and returns its
// contents along with a cleanup function releasing the mapping. The file can
// be closed right after the mapping is established.
func mmapReadOnly(path string) ([]byte, func() error, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	size := fi.Size()
	if size <= 0 {
		return nil, nil, fmt.Errorf("cannot mmap empty file %q", path)
	}
	if int64(int(size)) != size {
		return nil, nil, fmt.Errorf("file %q too large to mmap", path)
	}

	b, err := syscall.Mmap(int(f.Fd()), 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, nil, fmt.Errorf("mmap %q: %w", path, err)
	}
	return b, func() error { return syscall.Munmap(b) }, nil
}
