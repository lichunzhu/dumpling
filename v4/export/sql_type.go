package export

import (
	"bytes"
	"database/sql"
	"fmt"
	"strings"
)

var colTypeRowReceiverMap = map[string]func() RowReceiverStringer{}
var quotationMark byte = '\''

func init() {
	for _, s := range dataTypeString {
		colTypeRowReceiverMap[s] = SQLTypeStringMaker
	}
	for _, s := range dataTypeNum {
		colTypeRowReceiverMap[s] = SQLTypeNumberMaker
	}
	for _, s := range dataTypeBin {
		colTypeRowReceiverMap[s] = SQLTypeBytesMaker
	}
}

var dataTypeString = []string{
	"CHAR", "NCHAR", "VARCHAR", "NVARCHAR", "CHARACTER", "VARCHARACTER",
	"TIMESTAMP", "DATETIME", "DATE", "TIME", "YEAR", "SQL_TSI_YEAR",
	"TEXT", "TINYTEXT", "MEDIUMTEXT", "LONGTEXT",
	"ENUM", "SET", "JSON",
}

var dataTypeNum = []string{
	"INTEGER", "BIGINT", "TINYINT", "SMALLINT", "MEDIUMINT",
	"INT", "INT1", "INT2", "INT3", "INT8",
	"FLOAT", "REAL", "DOUBLE", "DOUBLE PRECISION",
	"DECIMAL", "NUMERIC", "FIXED",
	"BOOL", "BOOLEAN",
}

var dataTypeBin = []string{
	"BLOB", "TINYBLOB", "MEDIUMBLOB", "LONGBLOB", "LONG",
	"BINARY", "VARBINARY",
	"BIT",
}

type escapeInterface interface {
	Escape(string, *bytes.Buffer)
}

type backslashEscape struct{}

func (b backslashEscape) Escape(s string, bf *bytes.Buffer) {
	var (
		escape byte
		last   = 0
	)
	if bf.Len()+len(s) >= bf.Cap() {
		bf.Grow(2 * len(s))
	}
	// reference: https://gist.github.com/siddontang/8875771
	for i := 0; i < len(s); i++ {
		escape = 0

		switch s[i] {
		case 0: /* Must be escaped for 'mysql' */
			escape = '0'
			break
		case '\n': /* Must be escaped for logs */
			escape = 'n'
			break
		case '\r':
			escape = 'r'
			break
		case '\\':
			escape = '\\'
			break
		case '\'':
			escape = '\''
			break
		case '"': /* Better safe than sorry */
			escape = '"'
			break
		case '\032': /* This gives problems on Win32 */
			escape = 'Z'
		}

		if escape != 0 {
			bf.WriteString(s[last:i])
			bf.WriteByte('\\')
			bf.WriteByte(escape)
			last = i + 1
		}
	}
	if last == 0 {
		bf.WriteString(s)
		return
	} else if last < len(s) {
		bf.WriteString(s[last:])
	}
}

type noBackslashEscape struct{}

func (b noBackslashEscape) Escape(s string, bf *bytes.Buffer) {
	var (
		escape byte
		last   = 0
	)
	if bf.Len()+len(s) >= bf.Cap() {
		bf.Grow(2 * len(s))
	}
	for i := 0; i < len(s); i++ {
		escape = 0

		// `'` -> `''` and `\` -> `\\`
		switch s[i] {
		case '\\':
			escape = '\\'
			break
		case '\'':
			escape = '\''
			break
		}

		if escape != 0 {
			if last == 0 {
				bf.Grow(2 * len(s))
			}
			bf.WriteString(s[last : i+1])
			bf.WriteByte(escape)
			last = i + 1
		}
	}
	if last == 0 {
		bf.WriteString(s)
	} else if last < len(s) {
		bf.WriteString(s[last:])
	}
}

var globalEscape escapeInterface = backslashEscape{}

func SQLTypeStringMaker() RowReceiverStringer {
	return &SQLTypeString{}
}

func SQLTypeBytesMaker() RowReceiverStringer {
	return &SQLTypeBytes{}
}

func SQLTypeNumberMaker() RowReceiverStringer {
	return &SQLTypeNumber{}
}

func MakeRowReceiver(colTypes []string) RowReceiverStringer {
	rowReceiverArr := make(RowReceiverArr, len(colTypes))
	for i, colTp := range colTypes {
		recMaker, ok := colTypeRowReceiverMap[colTp]
		if !ok {
			recMaker = SQLTypeStringMaker
		}
		rowReceiverArr[i] = recMaker()
	}
	return rowReceiverArr
}

type RowReceiverArr []RowReceiverStringer

func (r RowReceiverArr) BindAddress(args []interface{}) {
	for i := range args {
		r[i].BindAddress(args[i : i+1])
	}
}
func (r RowReceiverArr) ReportSize() uint64 {
	var sum uint64
	for _, receiver := range r {
		sum += receiver.ReportSize()
	}
	return sum
}
func (r RowReceiverArr) ToString() string {
	var sb strings.Builder
	sb.WriteString("(")
	for i, receiver := range r {
		sb.WriteString(receiver.ToString())
		if i != len(r)-1 {
			sb.WriteString(", ")
		}
	}
	sb.WriteString(")")
	return sb.String()
}

func (r RowReceiverArr) WriteToStringBuilder(bf *buffPipe) {
	bf.bf.WriteString("(")
	for i, receiver := range r {
		receiver.WriteToStringBuilder(bf)
		if i != len(r)-1 {
			bf.bf.WriteString(",")
		}
	}
	bf.bf.WriteString(")")
}

type SQLTypeNumber struct {
	SQLTypeString
}

func (s SQLTypeNumber) ToString() string {
	if s.Valid {
		return s.String
	} else {
		return "NULL"
	}
}

func (s SQLTypeNumber) WriteToStringBuilder(bf *buffPipe) {
	if s.Valid {
		bf.bf.WriteString(s.String)
	} else {
		bf.bf.WriteString("NULL")
	}
}

type SQLTypeString struct {
	sql.NullString
}

func (s *SQLTypeString) BindAddress(arg []interface{}) {
	arg[0] = s
}
func (s *SQLTypeString) ReportSize() uint64 {
	if s.Valid {
		return uint64(len(s.String))
	}
	return uint64(len("NULL"))
}
func (s *SQLTypeString) ToString() string {
	if s.Valid {
		return fmt.Sprintf(`'%s'`, escape(s.String))
	} else {
		return "NULL"
	}
}

func (s *SQLTypeString) WriteToStringBuilder(bf *buffPipe) {
	if s.Valid {
		bf.bf.WriteByte(quotationMark)
		globalEscape.Escape(s.String, bf.bf)
		bf.bf.WriteByte(quotationMark)
	} else {
		bf.bf.WriteString("NULL")
	}
}

func escape(src string) string {
	src = strings.ReplaceAll(src, "'", "''")
	return strings.ReplaceAll(src, `\`, `\\`)
}

type SQLTypeBytes struct {
	bytes []byte
}

func (s *SQLTypeBytes) BindAddress(arg []interface{}) {
	arg[0] = &s.bytes
}
func (s *SQLTypeBytes) ReportSize() uint64 {
	return uint64(len(s.bytes))
}
func (s *SQLTypeBytes) ToString() string {
	return fmt.Sprintf("x'%x'", s.bytes)
}

func (s *SQLTypeBytes) WriteToStringBuilder(bf *buffPipe) {
	bf.bf.WriteString(fmt.Sprintf("x'%x'", s.bytes))
}
