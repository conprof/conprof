// Copyright 2024-2026 The Parca Authors
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

package profile

import (
	"fmt"

	"github.com/dennwc/varint"
)

type Demangler interface {
	Demangle(name []byte) string
}

type SymbolizationInfo struct {
	Addr    uint64
	BuildID []byte
	Mapping Mapping
}

// DecodeSymbolizationInfo decodes the address and mapping of an encoded
// location.
//
// Every read is bounds-checked, and a record that runs out mid-field yields
// whatever had been decoded before it did. It parses bytes handed to it by a
// caller -- on the ClickHouse ingest path, straight out of an Arrow dictionary
// buffer -- and there is no recovery interceptor there, so an unchecked index
// would take the process down rather than fail one request. Reading past the
// end is not even reliably a crash: an Arrow value's cap runs to the end of
// the whole buffer, so an over-read can silently return the bytes of the next
// location instead of panicking.
func DecodeSymbolizationInfo(data []byte) (SymbolizationInfo, uint64) {
	offset := 0

	// uvarint reports false when the record has run out or the varint is
	// malformed; Uvarint returns n <= 0 for both.
	uvarint := func() (uint64, bool) {
		if offset >= len(data) {
			return 0, false
		}
		v, n := varint.Uvarint(data[offset:])
		if n <= 0 {
			return 0, false
		}
		offset += n
		return v, true
	}
	// str reads a length-prefixed string. The length is unsigned, so one above
	// MaxInt converts to a negative int and offset+int(length) lands BELOW
	// offset -- which slips past a naive upper-bound check straight into a
	// panicking slice. Compare in the space the length was read in.
	str := func() ([]byte, bool) {
		length, ok := uvarint()
		if !ok || length > uint64(len(data)-offset) {
			return nil, false
		}
		v := data[offset : offset+int(length)]
		offset += int(length)
		return v, true
	}

	// We need to know the address size to read the build ID.
	addr, ok := uvarint()
	if !ok {
		return SymbolizationInfo{}, 0
	}

	numberOfLines, ok := uvarint()
	if !ok {
		return SymbolizationInfo{Addr: addr}, 0
	}

	if offset >= len(data) {
		return SymbolizationInfo{Addr: addr}, numberOfLines
	}
	hasMapping := data[offset] == 0x1
	offset++

	if !hasMapping {
		return SymbolizationInfo{Addr: addr}, numberOfLines
	}

	buildID, ok := str()
	if !ok {
		return SymbolizationInfo{Addr: addr}, numberOfLines
	}
	file, ok := str()
	if !ok {
		return SymbolizationInfo{Addr: addr, BuildID: buildID}, numberOfLines
	}
	memoryStart, ok := uvarint()
	if !ok {
		return SymbolizationInfo{
			Addr:    addr,
			BuildID: buildID,
			Mapping: Mapping{File: string(file)},
		}, numberOfLines
	}
	memoryLength, ok := uvarint()
	if !ok {
		return SymbolizationInfo{
			Addr:    addr,
			BuildID: buildID,
			Mapping: Mapping{StartAddr: memoryStart, File: string(file)},
		}, numberOfLines
	}
	// The final field is read without a success check on purpose: a zero offset
	// is the same answer a missing one would give.
	mappingOffset, _ := uvarint()

	return SymbolizationInfo{
		Addr:    addr,
		BuildID: buildID,
		Mapping: Mapping{
			StartAddr: memoryStart,
			EndAddr:   memoryStart + memoryLength,
			Offset:    mappingOffset,
			File:      string(file),
		},
	}, numberOfLines
}

type DecodeResult struct {
	WroteLines bool
	BuildID    []byte
	Addr       uint64
	Mapping    Mapping
}

func DecodeInto(lw LocationsWriter, data []byte, demangler Demangler) (DecodeResult, error) {
	var (
		n             int
		buildID       []byte
		memoryStart   uint64
		memoryLength  uint64
		mappingOffset uint64
	)

	addr, offset := varint.Uvarint(data)

	lineNumber, n := varint.Uvarint(data[offset:])
	offset += n

	hasMapping := data[offset] == 0x1
	offset++
	if hasMapping {
		buildID, n = decodeString(data[offset:])
		offset += n

		if err := lw.MappingBuildID.Append(buildID); err != nil {
			return DecodeResult{}, fmt.Errorf("append mapping build id: %w", err)
		}

		filename, n := decodeString(data[offset:])
		offset += n

		if err := lw.MappingFile.Append(filename); err != nil {
			return DecodeResult{}, fmt.Errorf("append mapping filename: %w", err)
		}

		memoryStart, n = varint.Uvarint(data[offset:])
		offset += n

		lw.MappingStart.Append(memoryStart)

		memoryLength, n = varint.Uvarint(data[offset:])
		offset += n

		lw.MappingLimit.Append(memoryStart + memoryLength)

		mappingOffset, n = varint.Uvarint(data[offset:])
		offset += n

		lw.MappingOffset.Append(mappingOffset)
	} else {
		lw.MappingStart.AppendNull()
		lw.MappingLimit.AppendNull()
		lw.MappingOffset.AppendNull()
		lw.MappingFile.AppendNull()
		lw.MappingBuildID.AppendNull()
	}

	if lineNumber > 0 {
		lw.Lines.Append(true)

		for i := uint64(0); i < lineNumber; i++ {
			lw.Line.Append(true)

			line, n := varint.Uvarint(data[offset:])
			offset += n

			lw.LineNumber.Append(int64(line))

			column, n := varint.Uvarint(data[offset:])
			offset += n
			lw.ColumnNumber.Append(column)

			hasFunction := data[offset] == 0x1
			offset++

			if hasFunction {
				startLine, n := varint.Uvarint(data[offset:])
				offset += n

				lw.FunctionStartLine.Append(int64(startLine))

				name, n := decodeString(data[offset:])
				offset += n

				systemName, n := decodeString(data[offset:])
				offset += n

				// Data written by the v2 ingest path before it populated the
				// name only carries system_name.
				if len(name) == 0 {
					name = systemName
				}

				if demangler != nil {
					name = []byte(demangler.Demangle(name))
				}

				if err := lw.FunctionName.Append([]byte(name)); err != nil {
					return DecodeResult{}, fmt.Errorf("append function name: %w", err)
				}

				if err := lw.FunctionSystemName.Append(systemName); err != nil {
					return DecodeResult{}, fmt.Errorf("append function system name: %w", err)
				}

				filename, n := decodeString(data[offset:])
				offset += n

				if err := lw.FunctionFilename.Append(filename); err != nil {
					return DecodeResult{}, fmt.Errorf("append function filename: %w", err)
				}
			} else {
				lw.FunctionStartLine.AppendNull()
				lw.FunctionName.AppendNull()
				lw.FunctionSystemName.AppendNull()
				lw.FunctionFilename.AppendNull()
			}
		}

		return DecodeResult{
			WroteLines: true,
		}, nil
	} else {
		return DecodeResult{
			WroteLines: false,
			BuildID:    buildID,
			Addr:       addr,
			Mapping: Mapping{
				StartAddr: memoryStart,
				EndAddr:   memoryStart + memoryLength,
				Offset:    mappingOffset,
			},
		}, nil
	}
}

// DecodeFunctionName is a fork of DecodeInto that only tries to find a function name and returns it.
// It returns "" if no function name is found.
func DecodeFunctionName(data []byte) ([]byte, error) {
	var n int

	// addr
	_, offset := varint.Uvarint(data)

	lineNumber, n := varint.Uvarint(data[offset:])
	offset += n

	hasMapping := data[offset] == 0x1
	offset++
	if hasMapping {
		// buildID
		_, n = decodeString(data[offset:])
		offset += n

		// filename
		_, n := decodeString(data[offset:])
		offset += n

		// memoryStart
		_, n = varint.Uvarint(data[offset:])
		offset += n

		// memoryLength
		_, n = varint.Uvarint(data[offset:])
		offset += n

		// mappingOffset
		_, n = varint.Uvarint(data[offset:])
		offset += n
	}

	if lineNumber > 0 {
		for i := uint64(0); i < lineNumber; i++ {
			// line
			_, n = varint.Uvarint(data[offset:])
			offset += n

			_, n = varint.Uvarint(data[offset:])
			offset += n

			hasFunction := data[offset] == 0x1
			offset++

			if hasFunction {
				// startLine
				_, n = varint.Uvarint(data[offset:])
				offset += n

				name, n := decodeString(data[offset:])
				if len(name) == 0 {
					// Fall back to system_name, matching DecodeInto.
					name, _ = decodeString(data[offset+n:])
				}
				return name, nil
			}
		}

		return []byte{}, nil
	} else {
		return []byte{}, nil
	}
}

func decodeString(data []byte) ([]byte, int) {
	length, n := varint.Uvarint(data)
	return data[n : n+int(length)], n + int(length)
}
