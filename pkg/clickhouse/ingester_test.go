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

package clickhouse

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"
	pprofextended "go.opentelemetry.io/proto/otlp/profiles/v1development"

	pprofpb "github.com/parca-dev/parca/gen/proto/go/google/pprof"
	"github.com/parca-dev/parca/pkg/profile"
)

// decodeLineInfo must stay in step with the location encoders, which write a
// column between the line number and the hasFunction flag.
//
// Every encoder in pkg/profile agrees on that layout -- EncodePprofLocation,
// EncodeOtelLocation, EncodeArrowLocation and the normalizer's v2 encoder --
// and so does the canonical decoder, profile.DecodeInto. A decoder that skips
// the column reads the column's own bytes as the flag, which costs the whole
// function block: name, system name, filename and start line.
//
// These drive the real encoders rather than hand-built blobs on purpose. A blob
// written by hand would be written from whatever the decoder happens to do, and
// so would agree with any bug it was supposed to catch.
func TestDecodeLineInfoRoundTripsTheEncodedLocation(t *testing.T) {
	const (
		wantName  = "bufio.(*Reader).Peek"
		wantSys   = "bufio.(*Reader).Peek"
		wantFile  = "/usr/local/go/src/bufio/bufio.go"
		wantLine  = 42
		wantStart = 10
	)

	// pprof carries no column information, so EncodePprofLocation writes a
	// uvarint zero -- a single 0x00 byte. The mapping block sits between the
	// line count and the lines, so a decoder that is wrong about one can
	// easily be right about the other; cover both. Values are non-zero so the
	// uvarints are more than one byte each.
	t.Run("pprof", func(t *testing.T) {
		stringTable := []string{"", wantName, wantSys, wantFile, "build-id", "/bin/svc"}
		funcs := []*pprofpb.Function{{Id: 1, Name: 1, SystemName: 2, Filename: 3, StartLine: wantStart}}

		for _, tc := range []struct {
			name    string
			mapping *pprofpb.Mapping
		}{
			{"with a mapping", &pprofpb.Mapping{
				Id: 1, BuildId: 4, Filename: 5,
				MemoryStart: 0x1000, MemoryLimit: 0x2000, FileOffset: 8,
			}},
			{"without a mapping", nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				loc := &pprofpb.Location{
					Id: 1, Address: 0xdeadbeef,
					Line: []*pprofpb.Line{{FunctionId: 1, Line: wantLine}},
				}
				if tc.mapping != nil {
					loc.MappingId = tc.mapping.Id
				}

				got, ok := decodeLineInfo(profile.EncodePprofLocation(loc, tc.mapping, funcs, stringTable))
				require.True(t, ok, "a well-formed location must decode cleanly")

				require.Equal(t, wantName, got.FunctionName, "the function name was discarded")
				require.Equal(t, wantSys, got.FunctionSystemName)
				require.Equal(t, wantFile, got.FunctionFilename)
				require.EqualValues(t, wantLine, got.LineNumber)
				require.EqualValues(t, wantStart, got.FunctionStartLine)
			})
		}
	})

	// A zero column is a single 0x00 byte, which cannot tell a uvarint read
	// apart from a bare offset++. OTLP locations carry real column numbers, so
	// they are what pins the read down -- and a multi-byte column (>= 0x80) is
	// what pins down that it is a *uvarint* read rather than a one-byte skip.
	t.Run("otel with a non-zero column", func(t *testing.T) {
		stringTable := []string{"", wantName, wantSys, wantFile, "build-id", "/bin/svc"}
		funcs := []*pprofextended.Function{{
			NameStrindex: 1, SystemNameStrindex: 2, FilenameStrindex: 3, StartLine: wantStart,
		}}

		for _, column := range []int64{1, 7, 300, 16384} {
			t.Run("column", func(t *testing.T) {
				loc := &pprofextended.Location{
					Address: 0xdeadbeef,
					Lines:   []*pprofextended.Line{{FunctionIndex: 1, Line: wantLine, Column: column}},
				}

				got, ok := decodeLineInfo(profile.EncodeOtelLocation(nil, loc, nil, funcs, stringTable))
				require.True(t, ok, "a well-formed location must decode cleanly")

				require.Equal(t, wantName, got.FunctionName, "column %d desynchronised the decoder", column)
				require.Equal(t, wantSys, got.FunctionSystemName)
				require.Equal(t, wantFile, got.FunctionFilename)
				require.EqualValues(t, wantLine, got.LineNumber)
				require.EqualValues(t, wantStart, got.FunctionStartLine)
			})
		}
	})

	// A line with no function is a representable shape: the encoder writes the
	// flag as 0x0 and no function block. The fields must come back zeroed --
	// which is the truth about this location, not a dropped name.
	t.Run("line without a function", func(t *testing.T) {
		loc := &pprofpb.Location{
			Id: 1, Address: 0xdeadbeef,
			Line: []*pprofpb.Line{{FunctionId: 0, Line: wantLine}},
		}

		got, ok := decodeLineInfo(profile.EncodePprofLocation(loc, nil, nil, []string{""}))
		require.True(t, ok, "a line with no function is a valid shape, not a malformed record")

		require.EqualValues(t, wantLine, got.LineNumber)
		require.Empty(t, got.FunctionName)
		require.Empty(t, got.FunctionSystemName)
		require.Empty(t, got.FunctionFilename)
		require.Zero(t, got.FunctionStartLine)
	})
}

// A truncated record must not panic.
//
// decodeLineInfo indexes and slices caller-supplied bytes, and the ClickHouse
// ingest path has no recovery interceptor, so a panic here is not a failed
// request -- it is a dead server. The bytes come from this server's own
// encoders, so the realistic way to get a malformed one is encoder/decoder
// drift, which is precisely the situation in which the decoder is already
// walking the record wrongly.
//
// Every prefix of a real encoded record is fed in, so the assertion covers
// running out mid-varint, mid-string, and exactly on a boundary, at every field
// in the layout rather than at a hand-picked few.
func TestDecodeLineInfoSurvivesTruncation(t *testing.T) {
	stringTable := []string{"", "main.main", "main.main", "/x/main.go", "build-id", "/bin/svc"}
	funcs := []*pprofpb.Function{{Id: 1, Name: 1, SystemName: 2, Filename: 3, StartLine: 10}}
	mapping := &pprofpb.Mapping{
		Id: 1, BuildId: 4, Filename: 5,
		MemoryStart: 0x1000, MemoryLimit: 0x2000, FileOffset: 8,
	}
	withFunc := []*pprofpb.Line{{FunctionId: 1, Line: 42}}

	// Each shape reaches a different set of reads, and the unguarded decoder
	// faulted in all of them: 55 of the 66 prefixes of the full record, and
	// 13 of 14 for the no-function one.
	for _, tc := range []struct {
		name    string
		mapping *pprofpb.Mapping
		lines   []*pprofpb.Line
	}{
		{"mapping and function", mapping, withFunc},
		{"no mapping", nil, withFunc},
		{"no function", mapping, []*pprofpb.Line{{FunctionId: 0, Line: 42}}},
		{"no lines", mapping, nil},
		{"no lines, no mapping", nil, nil},
		{"several lines", mapping, []*pprofpb.Line{
			{FunctionId: 1, Line: 42}, {FunctionId: 1, Line: 43}, {FunctionId: 1, Line: 44},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loc := &pprofpb.Location{Id: 1, Address: 0xdeadbeef, Line: tc.lines}
			if tc.mapping != nil {
				loc.MappingId = tc.mapping.Id
			}
			encoded := profile.EncodePprofLocation(loc, tc.mapping, funcs, stringTable)
			require.NotEmpty(t, encoded)

			// encoded[:i:i], not encoded[:i]: a two-index slice keeps the
			// original cap, and Go does not panic reading past len while still
			// inside cap -- so the cheaper spelling silently passes on inputs
			// that really do over-read. Capping is what makes this measure the
			// bound. It is not academic here: in production these bytes come
			// from an Arrow dictionary buffer, whose values are sliced with cap
			// running to the end of the whole buffer, so an over-read returns
			// the next location's bytes rather than faulting.
			for i := 0; i <= len(encoded); i++ {
				require.NotPanicsf(t, func() { decodeLineInfo(encoded[:i:i]) },
					"panicked on the first %d of %d bytes", i, len(encoded))
			}

			// The whole record still decodes, so the bounds checks did not cost
			// a field.
			got, ok := decodeLineInfo(encoded)
			require.True(t, ok)
			if len(tc.lines) > 0 {
				require.EqualValues(t, 42, got.LineNumber)
			}
			if len(tc.lines) > 0 && tc.lines[0].FunctionId != 0 {
				require.Equal(t, "main.main", got.FunctionName)
				require.Equal(t, "/x/main.go", got.FunctionFilename)
				require.EqualValues(t, 10, got.FunctionStartLine)
			}
		})
	}
}

// A length prefix larger than the record must not panic.
//
// The length is read as a uint64. One above MaxInt converts to a NEGATIVE int,
// so offset+int(length) lands below offset -- which satisfies a naive
// "offset+int(length) > len(data)" check and then panics on a slice whose high
// bound is less than its low one. The check has to be made in the space the
// length was read in.
func TestDecodeLineInfoRejectsOversizedLength(t *testing.T) {
	for _, tc := range []struct {
		name   string
		length uint64
	}{
		{"longer than the record", 1 << 20},
		{"larger than MaxInt", ^uint64(0)},
		{"MaxInt64 plus one", 1 << 63},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf []byte
			buf = binary.AppendUvarint(buf, 0xdeadbeef) // address
			buf = binary.AppendUvarint(buf, 1)          // numLines
			buf = append(buf, 0x0)                      // hasMapping = false
			buf = binary.AppendUvarint(buf, 42)         // line number
			buf = binary.AppendUvarint(buf, 0)          // column
			buf = append(buf, 0x1)                      // hasFunction = true
			buf = binary.AppendUvarint(buf, 10)         // startLine
			buf = binary.AppendUvarint(buf, tc.length)  // function name length
			buf = append(buf, []byte("main.main")...)   // fewer bytes than claimed

			var got LineInfo
			var ok bool
			require.NotPanics(t, func() { got, ok = decodeLineInfo(buf) })
			require.False(t, ok, "an oversized length must be reported as malformed")
			// What was decoded before the bad length stands; the name does not.
			require.EqualValues(t, 42, got.LineNumber)
			require.EqualValues(t, 10, got.FunctionStartLine)
			require.Empty(t, got.FunctionName)
		})
	}
}

// The two mapping strings are read behind the hasMapping flag, which the
// function-string cases never reach, so they need their own oversized-length
// coverage.
func TestDecodeLineInfoRejectsOversizedMappingLength(t *testing.T) {
	for _, tc := range []struct {
		name  string
		which int // 0 = buildID, 1 = mapping filename
	}{
		{"build ID", 0},
		{"mapping filename", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf []byte
			buf = binary.AppendUvarint(buf, 0xdeadbeef) // address
			buf = binary.AppendUvarint(buf, 1)          // numLines
			buf = append(buf, 0x1)                      // hasMapping = true
			if tc.which == 0 {
				buf = binary.AppendUvarint(buf, ^uint64(0)) // buildID length
				buf = append(buf, []byte("short")...)
			} else {
				buf = binary.AppendUvarint(buf, 5) // buildID length
				buf = append(buf, []byte("bid01")...)
				buf = binary.AppendUvarint(buf, ^uint64(0)) // filename length
				buf = append(buf, []byte("short")...)
			}

			var ok bool
			require.NotPanics(t, func() { _, ok = decodeLineInfo(buf) })
			require.False(t, ok, "an oversized mapping length must be reported as malformed")
		})
	}
}
