// Copyright 2020 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package internal

import (
	"bufio"
	"fmt"
	"sort"
	"strings"

	"github.com/cloudspannerecosystem/harbourbridge/schema"
	"github.com/cloudspannerecosystem/harbourbridge/spanner/ddl"
)

// GenerateReport analyzes schema and data conversion stats and writes a
// detailed report to w and returns a brief summary (as a string).
func GenerateReport(fromPgDump bool, conv *Conv, w *bufio.Writer, badWrites map[string]int64) string {
	reports := analyzeTables(conv, badWrites)
	summary := generateSummary(conv, reports, badWrites)
	writeHeading(w, "Summary of Conversion")
	w.WriteString(summary)
	ignored := ignoredStatements(conv)
	w.WriteString("\n")
	if len(ignored) > 0 {
		justifyLines(w, fmt.Sprintf("Note that the following source DB statements "+
			"were detected but ignored: %s.",
			strings.Join(ignored, ", ")), 80, 0)
		w.WriteString("\n\n")
	}
	statementsMsg := ""
	if fromPgDump {
		statementsMsg = "stats on the pg_dump statements processed, followed by "
	}
	justifyLines(w, "The remainder of this report provides "+statementsMsg+
		"a table-by-table listing of schema and data conversion details. "+
		"For background on the schema and data conversion process used, "+
		"and explanations of the terms and notes used in this "+
		"report, see HarbourBridge's README.", 80, 0)
	w.WriteString("\n\n")
	if fromPgDump {
		writeStmtStats(conv, w)
	}
	for _, t := range reports {
		h := fmt.Sprintf("Table %s", t.srcTable)
		if t.srcTable != t.spTable {
			h = h + fmt.Sprintf(" (mapped to Spanner table %s)", t.spTable)
		}
		writeHeading(w, h)
		w.WriteString(rateConversion(t.rows, t.badRows, t.cols, t.warnings, t.syntheticPKey != "", false))
		w.WriteString("\n")
		for _, x := range t.body {
			fmt.Fprintf(w, "%s\n", x.heading)
			for i, l := range x.lines {
				justifyLines(w, fmt.Sprintf("%d) %s.\n", i+1, l), 80, 3)
			}
			w.WriteString("\n")
		}
	}
	writeUnexpectedConditions(conv, w)
	return summary
}

type tableReport struct {
	srcTable      string
	spTable       string
	rows          int64
	badRows       int64
	cols          int64
	warnings      int64
	syntheticPKey string // Empty string means no synthetic primary key was needed.
	body          []tableReportBody
}

type tableReportBody struct {
	heading string
	lines   []string
}

func analyzeTables(conv *Conv, badWrites map[string]int64) (r []tableReport) {
	// Process tables in alphabetical order. This ensures that tables
	// appear in alphabetical order in report.txt.
	var tables []string
	for t := range conv.srcSchema {
		tables = append(tables, t)
	}
	sort.Strings(tables)
	for _, srcTable := range tables {
		r = append(r, buildTableReport(conv, srcTable, badWrites))
	}
	return r
}

func buildTableReport(conv *Conv, srcTable string, badWrites map[string]int64) tableReport {
	spTable, err := GetSpannerTable(conv, srcTable)
	srcSchema, ok1 := conv.srcSchema[srcTable]
	spSchema, ok2 := conv.spSchema[spTable]
	tr := tableReport{srcTable: srcTable, spTable: spTable}
	if err != nil || !ok1 || !ok2 {
		m := "bad source-DB-to-Spanner table mapping or Spanner schema"
		conv.unexpected("report: " + m)
		tr.body = []tableReportBody{tableReportBody{heading: "Internal error: " + m}}
		return tr
	}
	issues, cols, warnings := analyzeCols(conv, srcTable, spTable)
	tr.cols = cols
	tr.warnings = warnings
	if pk, ok := conv.syntheticPKeys[spTable]; ok {
		tr.syntheticPKey = pk.col
		tr.body = buildTableReportBody(conv, srcTable, issues, spSchema, srcSchema, &pk.col)
	} else {
		tr.body = buildTableReportBody(conv, srcTable, issues, spSchema, srcSchema, nil)
	}
	fillRowStats(conv, srcTable, badWrites, &tr)
	return tr
}

func buildTableReportBody(conv *Conv, srcTable string, issues map[string][]schemaIssue, spSchema ddl.CreateTable, srcSchema schema.Table, syntheticPK *string) []tableReportBody {
	var body []tableReportBody
	for _, p := range []struct {
		heading  string
		severity severity
	}{
		{"Warning", warning},
		{"Note", note},
	} {
		// Print out issues is alphabetical column order.
		var cols []string
		for t := range issues {
			cols = append(cols, t)
		}
		sort.Strings(cols)
		var l []string
		if syntheticPK != nil {
			// Warnings about synthetic primary keys must be handled as a special case
			// because we have a Spanner column with no matching source DB col.
			// Much of the generic code for processing issues assumes we have both.
			if p.severity == warning {
				l = append(l, fmt.Sprintf("Column '%s' was added because this table didn't have a primary key. Spanner requires a primary key for every table", *syntheticPK))
			}
		}
		issueBatcher := make(map[schemaIssue]bool)
		for _, srcCol := range cols {
			for _, i := range issues[srcCol] {
				if issueDB[i].severity != p.severity {
					continue
				}
				if issueDB[i].batch {
					if issueBatcher[i] {
						// Have already reported a previous instance of this
						// (batched) issue, so skip this one.
						continue
					}
					issueBatcher[i] = true
				}
				spCol, err := GetSpannerCol(conv, srcTable, srcCol, true)
				if err != nil {
					conv.unexpected(err.Error())
				}
				srcType := printSourceType(srcSchema.ColDefs[srcCol].Type)
				spType := spSchema.ColDefs[spCol].PrintColumnDefType()
				// A note on case: Spanner types are case insensitive, but
				// default to upper case. In particular, the Spanner AST uses
				// upper case, so spType is upper case. Many source DBs
				// default to lower case. When printing source DB and
				// Spanner types for comparison purposes, this can be distracting.
				// Hence we switch to lower-case for Spanner types here.
				// TODO: add logic to choose case for Spanner types based
				// on case of srcType.
				spType = strings.ToLower(spType)
				switch i {
				case defaultValue:
					l = append(l, fmt.Sprintf("%s e.g. column '%s'", issueDB[i].brief, srcCol))
				case foreignKey:
					l = append(l, fmt.Sprintf("Column '%s' uses foreign keys which Spanner does not support", srcCol))
				case timestamp:
					// Avoid the confusing "timestamp is mapped to timestamp" message.
					l = append(l, fmt.Sprintf("Some columns have source DB type 'timestamp without timezone' which is mapped to Spanner type timestamp e.g. column '%s'. %s", srcCol, issueDB[i].brief))
				case widened:
					l = append(l, fmt.Sprintf("%s e.g. for column '%s', source DB type %s is mapped to Spanner type %s", issueDB[i].brief, srcCol, srcType, spType))
				default:
					l = append(l, fmt.Sprintf("Column '%s': type %s is mapped to %s. %s", srcCol, srcType, spType, issueDB[i].brief))
				}
			}
		}
		if len(l) == 0 {
			continue
		}
		heading := p.heading
		if len(l) > 1 {
			heading = heading + "s"
		}
		body = append(body, tableReportBody{heading: heading, lines: l})
	}
	return body
}

func fillRowStats(conv *Conv, srcTable string, badWrites map[string]int64, tr *tableReport) {
	rows := conv.stats.rows[srcTable]
	goodConvRows := conv.stats.goodRows[srcTable]
	badConvRows := conv.stats.badRows[srcTable]
	badRowWrites := badWrites[srcTable]
	// Note on rows:
	// rows: all rows we encountered during processing.
	// goodConvRows: rows we successfully converted.
	// badConvRows: rows we failed to convert.
	// badRowWrites: rows we converted, but could not write to Spanner.
	if rows != goodConvRows+badConvRows || badRowWrites > goodConvRows {
		conv.unexpected(fmt.Sprintf("Inconsistent row counts for table %s: %d %d %d %d\n", srcTable, rows, goodConvRows, badConvRows, badRowWrites))
	}
	tr.rows = rows
	tr.badRows = badConvRows + badRowWrites
}

// Provides a description and severity for each schema issue.
// Note on batch: for some issues, we'd like to report just the first instance
// in a table and suppress other instances i.e. adding more instances
// of the issue in the same table has little value and could be very noisy.
// This is controlled via 'batch': if true, we count only the first instance
// for assessing warnings, and we give only the first instance in the report.
// TODO: add links in these descriptions to further documentation
// e.g. for timestamp description.
var issueDB = map[schemaIssue]struct {
	brief    string // Short description of issue.
	severity severity
	batch    bool // Whether multiple instances of this issue are combined.
}{
	defaultValue:          {brief: "Some columns have default values which Spanner does not support", severity: warning, batch: true},
	foreignKey:            {brief: "Spanner does not support foreign keys", severity: warning},
	multiDimensionalArray: {brief: "Spanner doesn't support multi-dimensional arrays", severity: warning},
	noGoodType:            {brief: "No appropriate Spanner type", severity: warning},
	numeric:               {brief: "Spanner does not support numeric. This type mapping could lose precision and is not recommended for production use", severity: warning},
	numericThatFits:       {brief: "Spanner does not support numeric, but this type mapping preserves the numeric's specified precision", severity: note},
	serial:                {brief: "Spanner does not support autoincrementing types", severity: warning},
	timestamp:             {brief: "Spanner timestamp is closer to PostgreSQL timestamptz", severity: note, batch: true},
	widened:               {brief: "Some columns will consume more storage in Spanner", severity: note, batch: true},
}

type severity int

const (
	warning severity = iota
	note
)

// analyzeCols returns information about the quality of schema mappings
// for table 'srcTable'. It assumes 'srcTable' is in the conv.srcSchema map.
func analyzeCols(conv *Conv, srcTable, spTable string) (map[string][]schemaIssue, int64, int64) {
	srcSchema := conv.srcSchema[srcTable]
	m := make(map[string][]schemaIssue)
	warnings := int64(0)
	warningBatcher := make(map[schemaIssue]bool)
	// Note on how we count warnings when there are multiple warnings
	// per column and/or multiple warnings per table.
	// non-batched warnings: count at most one warning per column.
	// batched warnings: count at most one warning per table.
	for c, l := range conv.issues[srcTable] {
		colWarning := false
		m[c] = l
		for _, i := range l {
			switch {
			case issueDB[i].severity == warning && issueDB[i].batch:
				warningBatcher[i] = true
			case issueDB[i].severity == warning && !issueDB[i].batch:
				colWarning = true
			}
		}
		if colWarning {
			warnings++
		}
	}
	warnings += int64(len(warningBatcher))
	return m, int64(len(srcSchema.ColDefs)), warnings
}

// rateSchema returns an string summarizing the quality of source DB
// to Spanner schema conversion. 'cols' and 'warnings' are respectively
// the number of columns converted and the warnings encountered
// (both weighted by number of data rows).
// 'missingPKey' indicates whether the source DB schema had a primary key.
// 'summary' indicates whether this is a per-table rating or an overall
// summary rating.
func rateSchema(cols, warnings int64, missingPKey, summary bool) string {
	pkMsg := "missing primary key"
	if summary {
		pkMsg = "some missing primary keys"
	}
	switch {
	case cols == 0:
		return "NONE (no schema found)"
	case warnings == 0 && !missingPKey:
		return "EXCELLENT (all columns mapped cleanly)"
	case warnings == 0 && missingPKey:
		return fmt.Sprintf("GOOD (all columns mapped cleanly, but %s)", pkMsg)
	case good(cols, warnings) && !missingPKey:
		return "GOOD (most columns mapped cleanly)"
	case good(cols, warnings) && missingPKey:
		return fmt.Sprintf("GOOD (most columns mapped cleanly, but %s)", pkMsg)
	case ok(cols, warnings) && !missingPKey:
		return "OK (some columns did not map cleanly)"
	case ok(cols, warnings) && missingPKey:
		return fmt.Sprintf("OK (some columns did not map cleanly + %s)", pkMsg)
	case !missingPKey:
		return "POOR (many columns did not map cleanly)"
	default:
		return fmt.Sprintf("POOR (many columns did not map cleanly + %s)", pkMsg)
	}
}

func rateData(rows int64, badRows int64) string {
	s := fmt.Sprintf(" (%s%% of %d rows written to Spanner)", pct(rows, badRows), rows)
	switch {
	case rows == 0:
		return "NONE (no data rows found)"
	case badRows == 0:
		return fmt.Sprintf("EXCELLENT (all %d rows written to Spanner)", rows)
	case good(rows, badRows):
		return "GOOD" + s
	case ok(rows, badRows):
		return "OK" + s
	default:
		return "POOR" + s
	}
}

func good(total, badCount int64) bool {
	return badCount < total/20
}

func ok(total, badCount int64) bool {
	return badCount < total/3
}

func rateConversion(rows, badRows, cols, warnings int64, missingPKey, summary bool) string {
	return fmt.Sprintf("Schema conversion: %s.\n", rateSchema(cols, warnings, missingPKey, summary)) +
		fmt.Sprintf("Data conversion: %s.\n", rateData(rows, badRows))
}

func generateSummary(conv *Conv, r []tableReport, badWrites map[string]int64) string {
	cols := int64(0)
	warnings := int64(0)
	missingPKey := false
	for _, t := range r {
		weight := t.rows // Weight col data by how many rows in table.
		if weight == 0 { // Tables without data count as if they had one row.
			weight = 1
		}
		cols += t.cols * weight
		warnings += t.warnings * weight
		if t.syntheticPKey != "" {
			missingPKey = true
		}
	}
	// Don't use tableReport for rows/badRows stats because tableReport
	// provides per-table stats for each table in the schema i.e. it omits
	// rows for tables not in the schema. To handle this corner-case, use
	// the source of truth for row stats: conv.stats.
	rows := conv.Rows()
	badRows := conv.BadRows() // Bad rows encountered during data conversion.
	// Add in bad rows while writing to Spanner.
	for _, n := range badWrites {
		badRows += n
	}
	return rateConversion(rows, badRows, cols, warnings, missingPKey, true)
}

func ignoredStatements(conv *Conv) (l []string) {
	for s := range conv.stats.statement {
		switch s {
		case "CreateFunctionStmt":
			l = append(l, "functions")
		case "CreateSeqStmt":
			l = append(l, "sequences")
		case "CreatePLangStmt":
			l = append(l, "procedures")
		case "CreateTrigStmt":
			l = append(l, "triggers")
		case "IndexStmt":
			l = append(l, "(non-primary) indexes")
		case "ViewStmt":
			l = append(l, "views")
		}
	}
	sort.Strings(l)
	return l
}

func writeStmtStats(conv *Conv, w *bufio.Writer) {
	type stat struct {
		statement string
		count     int64
	}
	var l []stat
	for s, x := range conv.stats.statement {
		l = append(l, stat{s, x.schema + x.data + x.skip + x.error})
	}
	// Sort by alphabetical order of statements.
	sort.Slice(l, func(i, j int) bool {
		return l[i].statement < l[j].statement
	})
	writeHeading(w, "Statements Processed")
	w.WriteString("Analysis of statements in pg_dump output, broken down by statement type.\n")
	w.WriteString("  schema: statements successfully processed for Spanner schema information.\n")
	w.WriteString("    data: statements successfully processed for data.\n")
	w.WriteString("    skip: statements not relevant for Spanner schema or data.\n")
	w.WriteString("   error: statements that could not be processed.\n")
	w.WriteString("  --------------------------------------\n")
	fmt.Fprintf(w, "  %6s %6s %6s %6s  %s\n", "schema", "data", "skip", "error", "statement")
	w.WriteString("  --------------------------------------\n")
	for _, x := range l {
		s := conv.stats.statement[x.statement]
		fmt.Fprintf(w, "  %6d %6d %6d %6d  %s\n", s.schema, s.data, s.skip, s.error, x.statement)
	}
	w.WriteString("See github.com/lfittl/pg_query_go/nodes for definitions of statement types\n")
	w.WriteString("(lfittl/pg_query_go is the library we use for parsing pg_dump output).\n")
	w.WriteString("\n")
}

func writeUnexpectedConditions(conv *Conv, w *bufio.Writer) {
	reparseInfo := func() {
		if conv.stats.reparsed > 0 {
			fmt.Fprintf(w, "Note: there were %d pg_dump reparse events while looking for statement boundaries.\n\n", conv.stats.reparsed)
		}
	}
	writeHeading(w, "Unexpected Conditions")
	if len(conv.stats.unexpected) == 0 {
		w.WriteString("There were no unexpected conditions encountered during processing.\n\n")
		reparseInfo()
		return
	}
	w.WriteString("For debugging only. This section provides details of unexpected conditions\n")
	w.WriteString("encountered as we processed the pg_dump data. In particular, the AST node\n")
	w.WriteString("representation used by the lfittl/pg_query_go library used for parsing\n")
	w.WriteString("pg_dump output is highly permissive: almost any construct can appear at\n")
	w.WriteString("any node in the AST tree. The list details all unexpected nodes and\n")
	w.WriteString("conditions.\n")
	w.WriteString("  --------------------------------------\n")
	fmt.Fprintf(w, "  %6s  %s\n", "count", "condition")
	w.WriteString("  --------------------------------------\n")
	for s, n := range conv.stats.unexpected {
		fmt.Fprintf(w, "  %6d  %s\n", n, s)
	}
	w.WriteString("\n")
	reparseInfo()
}

// justifyLines writes s out to w, adding newlines between words
// to keep line length under 'limit'. Newlines are indented
// 'indent' spaces.
func justifyLines(w *bufio.Writer, s string, limit int, indent int) {
	n := 0
	startOfLine := true
	words := strings.Split(s, " ") // This only handles spaces (newlines, tabs ignored).
	for _, x := range words {
		if n+len(x) > limit && !startOfLine {
			w.WriteString("\n")
			w.WriteString(strings.Repeat(" ", indent))
			n = indent
			startOfLine = true
		}
		if startOfLine {
			w.WriteString(x)
			n += len(x)
		} else {
			w.WriteString(" " + x)
			n += len(x) + 1
		}
		startOfLine = false
	}
}

// pct prints a percentage representation of (total-bad)/total
func pct(total, bad int64) string {
	if bad == 0 || total == 0 {
		return "100"
	}
	pct := 100.0 * float64(total-bad) / float64(total)
	if pct > 99.9 {
		return fmt.Sprintf("%2.5f", pct)
	}
	if pct > 95.0 {
		return fmt.Sprintf("%2.3f", pct)
	}
	return fmt.Sprintf("%2.0f", pct)
}

func writeHeading(w *bufio.Writer, s string) {
	w.WriteString(strings.Join([]string{
		"----------------------------\n",
		s, "\n",
		"----------------------------\n"}, ""))
}
