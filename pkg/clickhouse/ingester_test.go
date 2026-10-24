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

				got := decodeLineInfo(profile.EncodePprofLocation(loc, tc.mapping, funcs, stringTable))

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

				got := decodeLineInfo(profile.EncodeOtelLocation(nil, loc, nil, funcs, stringTable))

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

		got := decodeLineInfo(profile.EncodePprofLocation(loc, nil, nil, []string{""}))

		require.EqualValues(t, wantLine, got.LineNumber)
		require.Empty(t, got.FunctionName)
		require.Empty(t, got.FunctionSystemName)
		require.Empty(t, got.FunctionFilename)
		require.Zero(t, got.FunctionStartLine)
	})
}
