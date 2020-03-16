package export

import (
	"bytes"
	"fmt"
	"strings"
)

var colTypeRowReceiverMap = map[string]func() RowReceiverStringer{}
var quotationMark byte = '\''
var quotationMarkNotQuote = []byte("'")
var quotationMarkQuote = []byte("''")

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

func escape(s []byte, bf *bytes.Buffer, escapeBackslash bool) {
	if !escapeBackslash {
		bf.Write(bytes.ReplaceAll(s, quotationMarkNotQuote, quotationMarkQuote))
		return
	}
	var (
		escape byte
		last   = 0
	)
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
			bf.Write(s[last:i])
			bf.WriteByte('\\')
			bf.WriteByte(escape)
			last = i + 1
		}
	}
	if last == 0 {
		bf.Write(s)
	} else if last < len(s) {
		bf.Write(s[last:])
	}
}

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
func (r RowReceiverArr) ToString(escapeBackslash bool) string {
	var sb strings.Builder
	sb.WriteByte('(')
	for i, receiver := range r {
		sb.WriteString(receiver.ToString(escapeBackslash))
		if i != len(r)-1 {
			sb.WriteByte(',')
		}
	}
	sb.WriteByte(')')
	return sb.String()
}

func (r RowReceiverArr) WriteToBuffer(bf *bytes.Buffer, escapeBackslash bool) {
	bf.WriteByte('(')
	for i, receiver := range r {
		receiver.WriteToBuffer(bf, escapeBackslash)
		if i != len(r)-1 {
			bf.WriteString(",")
		}
	}
	bf.WriteByte(')')
}

type SQLTypeNumber struct {
	SQLTypeString
}

func (s SQLTypeNumber) ToString(bool) string {
	if s.bytes != nil {
		return string(s.bytes)
	} else {
		return "NULL"
	}
}

func (s SQLTypeNumber) WriteToBuffer(bf *bytes.Buffer, _ bool) {
	if s.bytes != nil {
		bf.Write(s.bytes)
	} else {
		bf.WriteString("NULL")
	}
}

type SQLTypeString struct {
	bytes []byte
}

func (s *SQLTypeString) BindAddress(arg []interface{}) {
	arg[0] = &s.bytes
}
func (s *SQLTypeString) ReportSize() uint64 {
	if s.bytes != nil {
		return uint64(len(s.bytes))
	}
	return uint64(len("NULL"))
}
func (s *SQLTypeString) ToString(escapeBackslash bool) string {
	if s.bytes != nil {
		var bf bytes.Buffer
		bf.WriteByte(quotationMark)
		escape(s.bytes, &bf, escapeBackslash)
		bf.WriteByte(quotationMark)
		defer bf.Reset()
		return bf.String()
	} else {
		return "NULL"
	}
}

func (s *SQLTypeString) WriteToBuffer(bf *bytes.Buffer, escapeBackslash bool) {
	if s.bytes != nil {
		bf.WriteByte(quotationMark)
		escape(s.bytes, bf, escapeBackslash)
		bf.WriteByte(quotationMark)
	} else {
		bf.WriteString("NULL")
	}
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
func (s *SQLTypeBytes) ToString(bool) string {
	return fmt.Sprintf("x'%x'", s.bytes)
}

func (s *SQLTypeBytes) WriteToBuffer(bf *bytes.Buffer, _ bool) {
	bf.WriteString(fmt.Sprintf("x'%x'", s.bytes))
}
