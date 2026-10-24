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
		stacktraceData := extractStacktraceData(record, stacktraceIdx, row)

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
// It returns the first line's info (most profiles have one line per location).
func decodeLineInfo(data []byte) LineInfo {
	var n int
	info := LineInfo{}

	// Skip addr
	_, offset := varint.Uvarint(data)

	// Read number of lines
	numLines, n := varint.Uvarint(data[offset:])
	offset += n

	// Check if has mapping
	hasMapping := data[offset] == 0x1
	offset++

	if hasMapping {
		// Skip buildID
		length, n := varint.Uvarint(data[offset:])
		offset += n + int(length)

		// Skip filename
		length, n = varint.Uvarint(data[offset:])
		offset += n + int(length)

		// Skip memoryStart
		_, n = varint.Uvarint(data[offset:])
		offset += n

		// Skip memoryLength
		_, n = varint.Uvarint(data[offset:])
		offset += n

		// Skip mappingOffset
		_, n = varint.Uvarint(data[offset:])
		offset += n
	}

	if numLines > 0 {
		// Read first line info (we only store one line per location)
		lineNum, n := varint.Uvarint(data[offset:])
		offset += n
		info.LineNumber = int64(lineNum)

		// Read the column. pprof carries no column information, so
		// EncodePprofLocation writes a uvarint zero here -- a single 0x00 byte.
		// Leaving it unread makes the hasFunction read below land on the column
		// instead of the flag, where it is always false, which silently discards
		// the function name, system name, filename and start line of every
		// already-symbolized location.
		_, n = varint.Uvarint(data[offset:])
		offset += n

		hasFunction := data[offset] == 0x1
		offset++

		if hasFunction {
			// Read startLine
			startLine, n := varint.Uvarint(data[offset:])
			offset += n
			info.FunctionStartLine = int64(startLine)

			// Read function name
			length, n := varint.Uvarint(data[offset:])
			offset += n
			info.FunctionName = string(data[offset : offset+int(length)])
			offset += int(length)

			// Read system name
			length, n = varint.Uvarint(data[offset:])
			offset += n
			info.FunctionSystemName = string(data[offset : offset+int(length)])
			offset += int(length)

			// Read filename
			length, n = varint.Uvarint(data[offset:])
			offset += n
			info.FunctionFilename = string(data[offset : offset+int(length)])
		}
	}

	return info
}

// extractStacktraceData extracts stacktrace information from the encoded binary column.
// The stacktrace column contains encoded location data that needs to be decoded.
func extractStacktraceData(record arrow.RecordBatch, colIdx, row int) StacktraceData {
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
		return data
	}

	col := record.Column(colIdx)
	listCol, ok := col.(*array.List)
	if !ok {
		return data
	}

	if listCol.IsNull(row) {
		return data
	}

	start, end := listCol.ValueOffsets(row)
	values := listCol.ListValues()

	dictCol, ok := values.(*array.Dictionary)
	if !ok {
		return data
	}

	binaryDict, ok := dictCol.Dictionary().(*array.Binary)
	if !ok {
		return data
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
		lineInfo := decodeLineInfo(encodedLocation)
		data.LineNumbers = append(data.LineNumbers, lineInfo.LineNumber)
		data.FunctionNames = append(data.FunctionNames, lineInfo.FunctionName)
		data.FunctionSystemNames = append(data.FunctionSystemNames, lineInfo.FunctionSystemName)
		data.FunctionFilenames = append(data.FunctionFilenames, lineInfo.FunctionFilename)
		data.FunctionStartLines = append(data.FunctionStartLines, lineInfo.FunctionStartLine)
	}

	return data
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
