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

package elfutils

import (
	"debug/dwarf"
	"debug/elf"
	"fmt"
	"strings"
)

// LoadDWARFData loads the DWARF debug info of f.
//
// Where possible, uncompressed DWARF sections are served as read-only
// memory-mapped views of the underlying file instead of being copied into Go
// heap buffers (see https://github.com/parca-dev/parca/issues/6404). The
// returned cleanup function releases the mapping and must be called once the
// dwarf.Data is no longer needed; it is nil when the mmap path was not taken.
//
// The previous loading behavior (debug/elf's File.DWARF, which copies section
// contents into heap buffers) is retained for compressed sections, for inputs
// whose DWARF sections require relocations to be applied, and on platforms
// without mmap support.
func LoadDWARFData(f *elf.File, filename string) (*dwarf.Data, func() error, error) {
	if d, unmap, ok, err := mmapDWARFData(f, filename); err != nil || ok {
		return d, unmap, err
	}

	d, err := f.DWARF()
	if err != nil {
		return nil, nil, err
	}
	return d, nil, nil
}

// dwarfSection is a DWARF debug section along with its index in the ELF
// section table and its suffix (e.g. "info" for ".debug_info").
type dwarfSection struct {
	index  int
	suffix string
	sec    *elf.Section
}

// dwarfSections returns all sections of f that carry DWARF debug info.
func dwarfSections(f *elf.File) []dwarfSection {
	var out []dwarfSection
	for i, s := range f.Sections {
		suffix := dwarfSuffix(s)
		if suffix == "" {
			continue
		}
		out = append(out, dwarfSection{index: i, suffix: suffix, sec: s})
	}
	return out
}

// mmapEligible reports whether every DWARF section of f can be served from a
// read-only memory mapping of the file.
func mmapEligible(f *elf.File, secs []dwarfSection) bool {
	// debug/elf applies relocations to DWARF section bytes for non-ET_EXEC
	// files. A read-only mapping cannot be modified in place, so retain the
	// copying loader for inputs with relocations against DWARF sections.
	relocated := map[int]bool{}
	if f.Type != elf.ET_EXEC {
		for _, r := range f.Sections {
			if r.Type != elf.SHT_REL && r.Type != elf.SHT_RELA {
				continue
			}
			relocated[int(r.Info)] = true
		}
	}

	for _, ds := range secs {
		s := ds.sec
		if s.Type != elf.SHT_PROGBITS {
			return false
		}
		if s.Flags&elf.SHF_COMPRESSED != 0 || strings.HasPrefix(s.Name, ".zdebug_") {
			// Section.Data transparently decompresses such sections; a
			// mapping would expose the raw compressed bytes.
			return false
		}
		if relocated[ds.index] {
			return false
		}
	}
	return true
}

// mmapDWARFData builds dwarf.Data from read-only memory-mapped views of the
// file's DWARF sections. It reports ok=false when the mmap path is not
// applicable so the caller can fall back to the copying loader; a non-nil err
// means the input is corrupt and the error is propagated.
func mmapDWARFData(f *elf.File, filename string) (d *dwarf.Data, unmap func() error, ok bool, err error) {
	secs := dwarfSections(f)
	if len(secs) == 0 || filename == "" || !mmapEligible(f, secs) {
		return nil, nil, false, nil
	}

	m, unmap, err := mmapReadOnly(filename)
	if err != nil {
		return nil, nil, false, nil
	}
	// Release the mapping on any failure below (cleanup on initialization
	// failure); the caller owns it once we report success.
	success := false
	defer func() {
		if !success {
			_ = unmap()
		}
	}()

	// Mirror debug/elf.File.DWARF: first the sections debug/dwarf started
	// with, then DWARF4 .debug_types and DWARF5 sections.
	dat := map[string][]byte{"abbrev": nil, "info": nil, "str": nil, "line": nil, "ranges": nil}
	for _, ds := range secs {
		if _, ok := dat[ds.suffix]; !ok {
			continue
		}
		b, ok := sectionBytes(m, ds.sec)
		if !ok {
			// Section extends beyond the mapped file (e.g. truncated after
			// open); let the copying loader surface the proper error.
			return nil, nil, false, nil
		}
		dat[ds.suffix] = b
	}

	d, err = dwarf.New(dat["abbrev"], nil, nil, dat["info"], dat["line"], nil, dat["ranges"], dat["str"])
	if err != nil {
		return nil, nil, false, err
	}

	for _, ds := range secs {
		if _, ok := dat[ds.suffix]; ok {
			// Already handled.
			continue
		}
		b, ok := sectionBytes(m, ds.sec)
		if !ok {
			return nil, nil, false, nil
		}
		if ds.suffix == "types" {
			if err := d.AddTypes(fmt.Sprintf("types-%d", ds.index), b); err != nil {
				return nil, nil, false, err
			}
		} else if err := d.AddSection(".debug_"+ds.suffix, b); err != nil {
			return nil, nil, false, err
		}
	}

	success = true
	return d, unmap, true, nil
}

// sectionBytes returns the contents of s as a view into the memory mapping m.
// It reports ok=false when the section lies outside the mapped file.
func sectionBytes(m []byte, s *elf.Section) (b []byte, ok bool) {
	// Guard against overflow and out-of-bounds sections (e.g. a file that was
	// truncated after it was opened).
	if s.Size > uint64(len(m)) || s.Offset > uint64(len(m))-s.Size {
		return nil, false
	}
	off := int(s.Offset)
	return m[off : off+int(s.Size)], true
}
