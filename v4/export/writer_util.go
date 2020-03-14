package export

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/pingcap/dumpling/v4/log"
	"go.uber.org/zap"
)

const lengthLimit = 1048576

type Dao struct {
	bp sync.Pool
}

func NewDao() (d *Dao) {
	d = &Dao{
		bp: sync.Pool{
			New: func() interface{} {
				return &bytes.Buffer{}
			},
		},
	}
	return
}

type buffPipe struct {
	input chan string
	bf    *bytes.Buffer
}

func (b *buffPipe) Run() {
	for s := range b.input {
		b.bf.WriteString(s)
	}
}

type writerPipe struct {
	sync.Mutex

	input  chan []byte
	closed chan struct{}

	w   io.Writer
	err error
}

func (b *writerPipe) Run() {
	defer close(b.closed)
	for s := range b.input {
		if b.err != nil {
			return
		}
		err := writeBytes(b.w, s)
		if b.err != nil {
			b.Lock()
			b.err = err
			b.Unlock()
		}
	}
}

func (b *writerPipe) Error() error {
	b.Lock()
	defer b.Unlock()
	return b.err
}

func WriteMeta(meta MetaIR, w io.StringWriter) error {
	log.Zap().Debug("start dumping meta data", zap.String("target", meta.TargetName()))

	specCmtIter := meta.SpecialComments()
	for specCmtIter.HasNext() {
		if err := write(w, fmt.Sprintf("%s\n", specCmtIter.Next())); err != nil {
			return err
		}
	}

	if err := write(w, fmt.Sprintf("%s;\n", meta.MetaSQL())); err != nil {
		return err
	}

	log.Zap().Debug("finish dumping meta data", zap.String("target", meta.TargetName()))
	return nil
}

func WriteInsert(tblIR TableDataIR, w io.Writer) error {
	fileRowIter := tblIR.Rows()
	if !fileRowIter.HasNext(false) {
		return nil
	}

	var err error

	dao := NewDao()
	bf := dao.bp.Get().(*bytes.Buffer)
	bf.Grow(lengthLimit)
	bfp := &buffPipe{
		input: make(chan string, 1),
		bf:    bf,
	}
	wp := &writerPipe{
		input:  make(chan []byte, 8),
		closed: make(chan struct{}),
		w:      w,
	}
	defer close(bfp.input)
	// go bfp.Run()
	go wp.Run()
	specCmtIter := tblIR.SpecialComments()
	for specCmtIter.HasNext() {
		bf.WriteString(specCmtIter.Next())
		bf.WriteString("\n")
	}

	var (
		insertStatementPrefix = fmt.Sprintf("INSERT INTO %s VALUES\n", wrapBackTicks(tblIR.TableName()))
		row                   = MakeRowReceiver(tblIR.ColumnTypes())
		counter               = 0
	)

	selectedField := tblIR.SelectedField()
	// if has generated column
	if selectedField != "" {
		insertStatementPrefix = fmt.Sprintf("INSERT INTO %s %s VALUES\n",
			wrapBackTicks(tblIR.TableName()), selectedField)
	}

	for fileRowIter.HasNextSQLRowIter() {
		bfp.bf.WriteString(insertStatementPrefix)

		fileRowIter = fileRowIter.NextSQLRowIter()
		for fileRowIter.HasNext(false) {
			if err = fileRowIter.Next(row, true); err != nil {
				log.Zap().Error("scanning from sql.Row failed", zap.Error(err))
				return err
			}

			row.WriteToStringBuilder(bfp)
			counter += 1

			var splitter string
			if fileRowIter.HasNext(true) {
				splitter = ",\n"
			} else {
				splitter = ";\n"
			}
			bfp.bf.WriteString(splitter)

			if bf.Len() >= lengthLimit {
				wp.input <- bf.Bytes()
				bf.Reset()
			}
			if err = wp.Error(); err != nil {
				return err
			}
		}
	}
	log.Zap().Debug("dumping table",
		zap.String("table", tblIR.TableName()),
		zap.Int("record counts", counter))
	if bf.Len() > 0 {
		wp.input <- bf.Bytes()
		bf.Reset()
		dao.bp.Put(bf)
	}
	close(wp.input)
	<-wp.closed
	return wp.Error()
}

func write(writer io.StringWriter, str string) error {
	_, err := writer.WriteString(str)
	if err != nil {
		log.Zap().Error("writing failed",
			zap.String("string", str),
			zap.Error(err))
	}
	return err
}

func writeBytes(writer io.Writer, p []byte) error {
	_, err := writer.Write(p)
	if err != nil {
		log.Zap().Error("writing failed",
			zap.ByteString("string", p),
			zap.Error(err))
	}
	return err
}

func buildFileWriter(path string) (io.StringWriter, func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		log.Zap().Error("open file failed",
			zap.String("path", path),
			zap.Error(err))
		return nil, nil, err
	}
	log.Zap().Debug("opened file", zap.String("path", path))
	buf := bufio.NewWriter(file)
	tearDownRoutine := func() {
		_ = buf.Flush()
		err := file.Close()
		if err == nil {
			return
		}
		log.Zap().Error("close file failed",
			zap.String("path", path),
			zap.Error(err))
	}
	return buf, tearDownRoutine, nil
}

func buildLazyFileWriter(path string) (io.StringWriter, func()) {
	var file *os.File
	var buf *bufio.Writer
	lazyStringWriter := &LazyStringWriter{}
	initRoutine := func() error {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
		file = f
		if err != nil {
			log.Zap().Error("open file failed",
				zap.String("path", path),
				zap.Error(err))
		}
		log.Zap().Debug("opened file", zap.String("path", path))
		buf = bufio.NewWriter(file)
		lazyStringWriter.StringWriter = buf
		return err
	}
	lazyStringWriter.initRoutine = initRoutine

	tearDownRoutine := func() {
		if file == nil {
			return
		}
		log.Zap().Debug("tear down lazy file writer...")
		_ = buf.Flush()
		err := file.Close()
		if err == nil {
			return
		}
		log.Zap().Error("close file failed", zap.String("path", path))
	}
	return lazyStringWriter, tearDownRoutine
}

type LazyStringWriter struct {
	initRoutine func() error
	sync.Once
	io.StringWriter
	err error
}

func (l *LazyStringWriter) WriteString(str string) (int, error) {
	l.Do(func() { l.err = l.initRoutine() })
	if l.err != nil {
		return 0, fmt.Errorf("open file error: %s", l.err.Error())
	}
	return l.StringWriter.WriteString(str)
}

// InterceptStringWriter is an interceptor of io.StringWriter,
// tracking whether a StringWriter has written something.
type InterceptStringWriter struct {
	io.StringWriter
	SomethingIsWritten bool
}

func (w *InterceptStringWriter) WriteString(str string) (int, error) {
	if len(str) > 0 {
		w.SomethingIsWritten = true
	}
	return w.StringWriter.WriteString(str)
}

type InterceptBytesWriter struct {
	io.Writer
	SomethingIsWritten bool
}

func (w *InterceptBytesWriter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		w.SomethingIsWritten = true
	}
	return w.Writer.Write(p)
}

func wrapBackTicks(identifier string) string {
	if !strings.HasPrefix(identifier, "`") && !strings.HasSuffix(identifier, "`") {
		return wrapStringWith(identifier, "`")
	}
	return identifier
}

func wrapStringWith(str string, wrapper string) string {
	return fmt.Sprintf("%s%s%s", wrapper, str, wrapper)
}

func buildLazyBytesFileWriter(path string) (io.Writer, func()) {
	var file *os.File
	var buf *bufio.Writer
	lazyByteWriter := &LazyBytesWriter{}
	initRoutine := func() error {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
		file = f
		if err != nil {
			log.Zap().Error("open file failed",
				zap.String("path", path),
				zap.Error(err))
		}
		log.Zap().Debug("opened file", zap.String("path", path))
		buf = bufio.NewWriter(file)
		lazyByteWriter.Writer = buf
		return err
	}
	lazyByteWriter.initRoutine = initRoutine

	tearDownRoutine := func() {
		if file == nil {
			return
		}
		log.Zap().Debug("tear down lazy file writer...")
		_ = buf.Flush()
		err := file.Close()
		if err == nil {
			return
		}
		log.Zap().Error("close file failed", zap.String("path", path))
	}
	return lazyByteWriter, tearDownRoutine
}

type LazyBytesWriter struct {
	initRoutine func() error
	sync.Once
	io.Writer
	err error
}

func (l *LazyBytesWriter) Write(p []byte) (int, error) {
	l.Do(func() { l.err = l.initRoutine() })
	if l.err != nil {
		return 0, fmt.Errorf("open file error: %s", l.err.Error())
	}
	return l.Writer.Write(p)
}

func (l *LazyBytesWriter) WriteBytesBuffer(bf *bytes.Buffer) (int, error) {
	l.Do(func() { l.err = l.initRoutine() })
	if l.err != nil {
		return 0, fmt.Errorf("open file error: %s", l.err.Error())
	}
	return l.Writer.Write(bf.Bytes())
}
