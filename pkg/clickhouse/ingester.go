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

package clickhouse

import (
	"context"
	"fmt"
	"strings"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/dennwc/varint"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"

	"github.com/parca-dev/parca/pkg/profile"
)

// Ingester implements the ingester.Ingester interface for ClickHouse.
type Ingester struct {
	logger log.Logger
	client *Client
}

// NewIngester creates a new ClickHouse ingester.
func NewIngester(logger log.Logger, client *Client) *Ingester {
	return &Ingester{
		logger: logger,
		client: client,
	}
}

// Ingest implements the ingester.Ingester interface.
// It converts Arrow records to ClickHouse batch inserts.
func (i *Ingester) Ingest(ctx context.Context, record arrow.RecordBatch) error {
	if record.NumRows() == 0 {
		return nil
	}

	batch, err := i.client.PrepareBatch(ctx, InsertSQL(i.client.Database(), i.client.Table()))
	if err != nil {
		return fmt.Errorf("failed to prepare batch: %w", err)
	}

	schema := record.Schema()

	// Counts locations whose encoded bytes could not be fully decoded, so a
	// systematic encoder/decoder drift is visible instead of just producing
	// profiles with unnamed frames.
	malformedLocations := 0

	// Find column indices
	nameIdx := findColumnIndex(schema, profile.ColumnName)
	sampleTypeIdx := findColumnIndex(schema, profile.ColumnSampleType)
	sampleUnitIdx := findColumnIndex(schema, profile.ColumnSampleUnit)
	periodTypeIdx := findColumnIndex(schema, profile.ColumnPeriodType)
	periodUnitIdx := findColumnIndex(schema, profile.ColumnPeriodUnit)
	periodIdx := findColumnIndex(schema, profile.ColumnPeriod)
	durationIdx := findColumnIndex(schema, profile.ColumnDuration)
	timestampIdx := findColumnIndex(schema, profile.ColumnTimestamp)
	timeNanosIdx := findColumnIndex(schema, profile.ColumnTimeNanos)
	valueIdx := findColumnIndex(schema, profile.ColumnValue)
	stacktraceIdx := findColumnIndex(schema, profile.ColumnStacktrace)

	// Find label columns
	labelColumns := make(map[string]int)
	for idx, field := range schema.Fields() {
		if strings.HasPrefix(field.Name, profile.ColumnLabelsPrefix) {
			labelName := strings.TrimPrefix(field.Name, profile.ColumnLabelsPrefix)
			labelColumns[labelName] = idx
		}
	}

	for row := 0; row < int(record.NumRows()); row++ {
		// Extract profile metadata
		name := getStringValue(record, nameIdx, row)
		sampleType := getStringValue(record, sampleTypeIdx, row)
		sampleUnit := getStringValue(record, sampleUnitIdx, row)
		periodType := getStringValue(record, periodTypeIdx, row)
		periodUnit := getStringValue(record, periodUnitIdx, row)
		period := getInt64Value(record, periodIdx, row)
		duration := getInt64Value(record, durationIdx, row)
		timestamp := getInt64Value(record, timestampIdx, row)
		timeNanos := getInt64Value(record, timeNanosIdx, row)
		value := getInt64Value(record, valueIdx, row)

		// Extract labels as a map for JSON column
		labels := make(map[string]string)
		for labelName, colIdx := range labelColumns {
			if colIdx >= 0 {
				labelValue := getStringValue(record, colIdx, row)
				if labelValue != "" {
					labels[labelName] = labelValue
				}
			}
		}

		// Extract stacktrace data
		stacktraceData, malformed := extractStacktraceData(record, stacktraceIdx, row)
		malformedLocations += malformed

		// Append to batch
		err := batch.Append(
			name,
			sampleType,
			sampleUnit,
			periodType,
			periodUnit,
			period,
			duration,
			timestamp,
			timeNanos,
			value,
			labels,
			stacktraceData.Addresses,
			stacktraceData.MappingStarts,
			stacktraceData.MappingLimits,
			stacktraceData.MappingOffsets,
			stacktraceData.MappingFiles,
			stacktraceData.MappingBuildIDs,
			stacktraceData.LineNumbers,
			stacktraceData.FunctionNames,
			stacktraceData.FunctionSystemNames,
			stacktraceData.FunctionFilenames,
			stacktraceData.FunctionStartLines,
		)
		if err != nil {
			level.Error(i.logger).Log("msg", "failed to append row to batch", "err", err)
			return fmt.Errorf("failed to append row to batch: %w", err)
		}
	}

	// One line per batch, not per location: a drift affects every location in
	// the batch, and logging each one would bury the signal it is meant to be.
	if malformedLocations > 0 {
		level.Warn(i.logger).Log(
			"msg", "could not fully decode some encoded locations; their frames are stored without function info",
			"locations", malformedLocations,
			"rows", record.NumRows(),
		)
	}

	if err := batch.Send(); err != nil {
		return fmt.Errorf("failed to send batch: %w", err)
	}

	return nil
}

// StacktraceData holds the extracted stacktrace information for a single sample.
type StacktraceData struct {
	Addresses           []uint64
	MappingStarts       []uint64
	MappingLimits       []uint64
	MappingOffsets      []uint64
	MappingFiles        []string
	MappingBuildIDs     []string
	LineNumbers         []int64
	FunctionNames       []string
	FunctionSystemNames []string
	FunctionFilenames   []string
	FunctionStartLines  []int64
}

// LineInfo holds decoded line/function information from an encoded location.
type LineInfo struct {
	LineNumber         int64
	FunctionStartLine  int64
	FunctionName       string
	FunctionSystemName string
	FunctionFilename   string
}

// decodeLineInfo decodes line and function information from the encoded location data.
//
// Every read is bounds-checked, and a short or malformed record yields whatever
// had been decoded before the record ran out rather than panicking. The bytes
// are produced by this server's own encoders and so are self-consistent in
// normal operation, which is exactly why the failure mode matters: the realistic
// way to get a malformed record is encoder/decoder drift, and there is no
// recovery interceptor on the ingest path, so an unchecked index there takes the
// process down instead of failing one request.
func decodeLineInfo(data []byte) (LineInfo, bool) {
	info := LineInfo{}
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
	// str reads a length-prefixed string.
	str := func() (string, bool) {
		length, ok := uvarint()
		// The length is unsigned, so one larger than MaxInt converts to a
		// negative int and offset+int(length) lands BELOW offset -- which slips
		// past a naive offset+int(length) > len(data) check straight into a
		// panicking slice. Compare in the space the length was read in, against
		// the bytes that actually remain.
		if !ok || length > uint64(len(data)-offset) {
			return "", false
		}
		v := string(data[offset : offset+int(length)])
		offset += int(length)
		return v, true
	}
	flag := func() (bool, bool) {
		if offset >= len(data) {
			return false, false
		}
		v := data[offset] == 0x1
		offset++
		return v, true
	}
	skip := func(n int) bool {
		for range n {
			if _, ok := uvarint(); !ok {
				return false
			}
		}
		return true
	}

	if _, ok := uvarint(); !ok { // address
		return info, false
	}
	numLines, ok := uvarint()
	if !ok {
		return info, false
	}
	hasMapping, ok := flag()
	if !ok {
		return info, false
	}
	if hasMapping {
		if _, ok := str(); !ok { // buildID
			return info, false
		}
		if _, ok := str(); !ok { // filename
			return info, false
		}
		// memoryStart, memoryLength, mappingOffset
		if !skip(3) {
			return info, false
		}
	}

	// A location with no lines is a valid shape, not a malformed record.
	if numLines == 0 {
		return info, true
	}

	// Only the first line is kept: the schema stores one function per location.
	// Location.Line[0] is the innermost inlined function, which is the right one
	// to keep, but every inlined caller above it is dropped here.
	lineNum, ok := uvarint()
	if !ok {
		return info, false
	}
	info.LineNumber = int64(lineNum)

	// The column. pprof carries no column information, so EncodePprofLocation
	// writes a uvarint zero here -- a single 0x00 byte. Leaving it unread makes
	// the hasFunction read below land on the column instead of the flag, where
	// it is always false, which silently discards the function name, system
	// name, filename and start line of every already-symbolized location.
	if _, ok := uvarint(); !ok {
		return info, false
	}

	hasFunction, ok := flag()
	if !ok {
		return info, false
	}
	// A line with no function is a valid shape too.
	if !hasFunction {
		return info, true
	}

	startLine, ok := uvarint()
	if !ok {
		return info, false
	}
	info.FunctionStartLine = int64(startLine)
	if info.FunctionName, ok = str(); !ok {
		return info, false
	}
	if info.FunctionSystemName, ok = str(); !ok {
		return info, false
	}
	info.FunctionFilename, ok = str()
	return info, ok
}

// extractStacktraceData extracts stacktrace information from the encoded binary column.
// The stacktrace column contains encoded location data that needs to be decoded.
// The second return value counts locations whose encoded bytes could not be
// fully decoded. They are still written, with whatever was recovered, so one
// bad location does not discard the rest of the batch -- but the count is
// reported by the caller, because a decoder that silently degrades every
// profile to unnamed frames is indistinguishable from legitimately
// unsymbolized ones.
func extractStacktraceData(record arrow.RecordBatch, colIdx, row int) (StacktraceData, int) {
	malformed := 0
	data := StacktraceData{
		Addresses:           []uint64{},
		MappingStarts:       []uint64{},
		MappingLimits:       []uint64{},
		MappingOffsets:      []uint64{},
		MappingFiles:        []string{},
		MappingBuildIDs:     []string{},
		LineNumbers:         []int64{},
		FunctionNames:       []string{},
		FunctionSystemNames: []string{},
		FunctionFilenames:   []string{},
		FunctionStartLines:  []int64{},
	}

	if colIdx < 0 {
		return data, 0
	}

	col := record.Column(colIdx)
	listCol, ok := col.(*array.List)
	if !ok {
		return data, 0
	}

	if listCol.IsNull(row) {
		return data, 0
	}

	start, end := listCol.ValueOffsets(row)
	values := listCol.ListValues()

	dictCol, ok := values.(*array.Dictionary)
	if !ok {
		return data, 0
	}

	binaryDict, ok := dictCol.Dictionary().(*array.Binary)
	if !ok {
		return data, 0
	}

	for idx := int(start); idx < int(end); idx++ {
		if dictCol.IsNull(idx) {
			continue
		}

		dictIdx := dictCol.GetValueIndex(idx)
		encodedLocation := binaryDict.Value(dictIdx)

		// Decode the mapping info
		symInfo, _ := profile.DecodeSymbolizationInfo(encodedLocation)

		data.Addresses = append(data.Addresses, symInfo.Addr)
		data.MappingStarts = append(data.MappingStarts, symInfo.Mapping.StartAddr)
		data.MappingLimits = append(data.MappingLimits, symInfo.Mapping.EndAddr)
		data.MappingOffsets = append(data.MappingOffsets, symInfo.Mapping.Offset)
		data.MappingFiles = append(data.MappingFiles, symInfo.Mapping.File)
		data.MappingBuildIDs = append(data.MappingBuildIDs, string(symInfo.BuildID))

		// Decode line/function info
		lineInfo, ok := decodeLineInfo(encodedLocation)
		if !ok {
			malformed++
		}
		data.LineNumbers = append(data.LineNumbers, lineInfo.LineNumber)
		data.FunctionNames = append(data.FunctionNames, lineInfo.FunctionName)
		data.FunctionSystemNames = append(data.FunctionSystemNames, lineInfo.FunctionSystemName)
		data.FunctionFilenames = append(data.FunctionFilenames, lineInfo.FunctionFilename)
		data.FunctionStartLines = append(data.FunctionStartLines, lineInfo.FunctionStartLine)
	}

	return data, malformed
}

func findColumnIndex(schema *arrow.Schema, name string) int {
	indices := schema.FieldIndices(name)
	if len(indices) == 0 {
		return -1
	}
	return indices[0]
}

func getStringValue(record arrow.RecordBatch, colIdx, row int) string {
	if colIdx < 0 {
		return ""
	}

	col := record.Column(colIdx)
	if col.IsNull(row) {
		return ""
	}

	switch c := col.(type) {
	case *array.Dictionary:
		switch dict := c.Dictionary().(type) {
		case *array.Binary:
			return string(dict.Value(c.GetValueIndex(row)))
		case *array.String:
			return dict.Value(c.GetValueIndex(row))
		}
	case *array.String:
		return c.Value(row)
	case *array.Binary:
		return string(c.Value(row))
	}

	return ""
}

func getInt64Value(record arrow.RecordBatch, colIdx, row int) int64 {
	if colIdx < 0 {
		return 0
	}

	col := record.Column(colIdx)
	if col.IsNull(row) {
		return 0
	}

	switch c := col.(type) {
	case *array.Int64:
		return c.Value(row)
	case *array.Dictionary:
		switch dict := c.Dictionary().(type) {
		case *array.Int64:
			return dict.Value(c.GetValueIndex(row))
		}
	}

	return 0
}
