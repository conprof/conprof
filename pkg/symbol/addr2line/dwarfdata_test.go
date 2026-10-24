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
//

package addr2line

import (
	"context"
	"debug/dwarf"
	"debug/elf"
	"runtime"
	"testing"

	"github.com/go-kit/log"
	"github.com/stretchr/testify/require"

	"github.com/parca-dev/parca/pkg/symbol/demangle"
	"github.com/parca-dev/parca/pkg/symbol/elfutils"
)

func openTestELFFixture(t testing.TB) (*elf.File, string) {
	t.Helper()
	filename := "testdata/basic-cpp-no-fp-with-debuginfo"
	f, err := elf.Open(filename)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, f.Close()) })
	return f, filename
}

// countEntries counts every entry a fresh DWARF reader yields.
func countEntries(t *testing.T, d *dwarf.Data) int {
	t.Helper()
	r := d.Reader()
	n := 0
	for {
		e, err := r.Next()
		require.NoError(t, err)
		if e == nil {
			break
		}
		n++
	}
	return n
}

func unixOnly(t testing.TB) {
	t.Helper()
	switch runtime.GOOS {
	case "windows", "plan9", "js":
		t.Skipf("mmap-backed DWARF loading is not supported on %s", runtime.GOOS)
	}
}

func TestLoadDWARFDataMmap(t *testing.T) {
	unixOnly(t)

	f, filename := openTestELFFixture(t)

	d, unmap, err := elfutils.LoadDWARFData(f, filename)
	require.NoError(t, err)
	require.NotNil(t, d)
	// The fixture is an uncompressed ET_EXEC binary: the mmap path must be taken.
	require.NotNil(t, unmap, "expected mmap-backed DWARF loading for uncompressed sections")

	// The mmap-backed data must describe the same debug info as the copying loader.
	want, err := f.DWARF()
	require.NoError(t, err)
	require.Equal(t, countEntries(t, want), countEntries(t, d))
	require.Greater(t, countEntries(t, d), 0)

	require.NoError(t, unmap())
}

func TestLoadDWARFDataFallback(t *testing.T) {
	f, _ := openTestELFFixture(t)

	// Without a filename there is nothing to mmap: fall back to the copying loader.
	d, unmap, err := elfutils.LoadDWARFData(f, "")
	require.NoError(t, err)
	require.NotNil(t, d)
	require.Nil(t, unmap, "expected no mmap cleanup when falling back")

	want, err := f.DWARF()
	require.NoError(t, err)
	require.Equal(t, countEntries(t, want), countEntries(t, d))
}

func TestDwarfLinerMmapClose(t *testing.T) {
	unixOnly(t)

	logger := log.NewNopLogger()
	demangler := demangle.NewDemangler("simple", true)
	f, filename := openTestELFFixture(t)

	liner, err := DWARF(logger, filename, f, demangler)
	require.NoError(t, err)
	require.NotNil(t, liner.unmapDWARF, "expected mmap-backed DWARF loading")

	// Symbolization must work off the mapped sections.
	lines, err := liner.PCToLines(context.Background(), 0x401125)
	require.NoError(t, err)
	require.NotEmpty(t, lines)

	// Close must release the mapping without error.
	require.NoError(t, liner.Close())
}

func BenchmarkDWARFLoadStdlib(b *testing.B) {
	f, _ := openTestELFFixture(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d, err := f.DWARF()
		if err != nil {
			b.Fatal(err)
		}
		if d == nil {
			b.Fatal("nil dwarf data")
		}
	}
}

func BenchmarkDWARFLoadMmap(b *testing.B) {
	unixOnly(b)
	f, filename := openTestELFFixture(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d, unmap, err := elfutils.LoadDWARFData(f, filename)
		if err != nil {
			b.Fatal(err)
		}
		if d == nil {
			b.Fatal("nil dwarf data")
		}
		if unmap != nil {
			if err := unmap(); err != nil {
				b.Fatal(err)
			}
		}
	}
}
