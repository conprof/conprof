// Copyright 2026 The Parca Authors
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
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"

	pprofpb "github.com/parca-dev/parca/gen/proto/go/google/pprof"
)

// A truncated record must not panic.
//
// DecodeSymbolizationInfo is called on the ClickHouse ingest path, on bytes
// taken straight from an Arrow dictionary buffer, and there is no recovery
// interceptor there -- so an unchecked index is a dead server rather than a
// failed request. Before the bounds checks, 19 of the 66 prefixes of one real
// record faulted.
//
// Reading past the end is not even reliably a crash. array.Binary.Value slices
// with cap running to the end of the whole dictionary buffer, so an over-read
// can silently return the next location's bytes and store them as this
// location's mapping -- which is why the sweep caps each prefix with
// encoded[:i:i] rather than trusting a panic to reveal the bug.
func TestDecodeSymbolizationInfoSurvivesTruncation(t *testing.T) {
	stringTable := []string{"", "main.main", "main.main", "/x/main.go", "build-id", "/bin/svc"}
	funcs := []*pprofpb.Function{{Id: 1, Name: 1, SystemName: 2, Filename: 3, StartLine: 10}}

	for _, tc := range []struct {
		name    string
		mapping *pprofpb.Mapping
		lines   []*pprofpb.Line
	}{
		{"with a mapping", &pprofpb.Mapping{
			Id: 1, BuildId: 4, Filename: 5,
			MemoryStart: 0x1000, MemoryLimit: 0x2000, FileOffset: 8,
		}, []*pprofpb.Line{{FunctionId: 1, Line: 42}}},
		{"without a mapping", nil, []*pprofpb.Line{{FunctionId: 1, Line: 42}}},
		{"no lines", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loc := &pprofpb.Location{Id: 1, Address: 0xdeadbeef, Line: tc.lines}
			if tc.mapping != nil {
				loc.MappingId = tc.mapping.Id
			}
			encoded := EncodePprofLocation(loc, tc.mapping, funcs, stringTable)
			require.NotEmpty(t, encoded)

			for i := 0; i <= len(encoded); i++ {
				require.NotPanicsf(t, func() { DecodeSymbolizationInfo(encoded[:i:i]) },
					"panicked on the first %d of %d bytes", i, len(encoded))
			}

			// The whole record still decodes, so the bounds checks did not cost
			// a field.
			got, numLines := DecodeSymbolizationInfo(encoded)
			require.EqualValues(t, 0xdeadbeef, got.Addr)
			require.EqualValues(t, len(tc.lines), numLines)
			if tc.mapping != nil {
				require.Equal(t, "build-id", string(got.BuildID))
				require.Equal(t, "/bin/svc", got.Mapping.File)
				require.EqualValues(t, 0x1000, got.Mapping.StartAddr)
				require.EqualValues(t, 0x2000, got.Mapping.EndAddr)
				require.EqualValues(t, 8, got.Mapping.Offset)
			}
		})
	}
}

// A length prefix larger than the record must not panic. The length is
// unsigned: one above MaxInt converts to a negative int, so offset+int(length)
// lands below offset and satisfies a naive upper-bound check before panicking
// on a slice whose high bound is below its low one.
func TestDecodeSymbolizationInfoRejectsOversizedLength(t *testing.T) {
	for _, length := range []uint64{1 << 20, 1 << 63, ^uint64(0)} {
		var buf []byte
		buf = binary.AppendUvarint(buf, 0xdeadbeef) // address
		buf = binary.AppendUvarint(buf, 1)          // numLines
		buf = append(buf, 0x1)                      // hasMapping = true
		buf = binary.AppendUvarint(buf, length)     // buildID length
		buf = append(buf, []byte("short")...)       // fewer bytes than claimed

		var got SymbolizationInfo
		require.NotPanics(t, func() { got, _ = DecodeSymbolizationInfo(buf) })
		// The address was decoded before the bad length; the mapping was not.
		require.EqualValues(t, 0xdeadbeef, got.Addr)
		require.Empty(t, got.BuildID)
		require.Empty(t, got.Mapping.File)
	}
}
