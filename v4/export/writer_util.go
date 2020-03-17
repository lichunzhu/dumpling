package export

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"unsafe"

	"github.com/pingcap/dumpling/v4/log"
	"go.uber.org/zap"
)

const lengthLimit = 1048576

func bytes2str(b []byte) string {
	return *(*string)(unsafe.Pointer(&b))
}

type writerPipe struct {
	input  chan []byte
	closed chan struct{}
	errCh  chan error

	w io.StringWriter
}

func newWriterPipe(w io.StringWriter) *writerPipe {
	return &writerPipe{
		input:  make(chan []byte, 8),
		closed: make(chan struct{}),
		errCh:  make(chan error, 1),
		w:      w,
	}
}

func (b *writerPipe) Run(ctx context.Context) {
	defer close(b.closed)
	var errOccurs bool
	for {
		select {
		case s, ok := <-b.input:
			if !ok {
				return
			}
			if errOccurs {
				continue
			}
			err := write(b.w, bytes2str(s))
			if err != nil {
				errOccurs = true
				b.errCh <- err
			}
		case <-ctx.Done():
			return
		}
	}
}

func (b *writerPipe) Error() error {
	select {
	case err := <-b.errCh:
		return err
	default:
		return nil
	}
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

func WriteInsert(tblIR TableDataIR, w io.StringWriter) error {
	fileRowIter := tblIR.Rows()
	if !fileRowIter.HasNext() {
		return nil
	}

	pool := sync.Pool{New: func() interface{} {
		return &bytes.Buffer{}
	}}
	bf := pool.Get().(*bytes.Buffer)
	bf.Grow(lengthLimit)

	wp := newWriterPipe(w)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		wp.Run(ctx)
		wg.Done()
	}()
	defer func() {
		cancel()
		wg.Wait()
	}()

	specCmtIter := tblIR.SpecialComments()
	for specCmtIter.HasNext() {
		bf.WriteString(specCmtIter.Next())
		bf.WriteByte('\n')
	}

	var (
		insertStatementPrefix string
		row                   = MakeRowReceiver(tblIR.ColumnTypes())
		counter               = 0
		escapeBackSlash       = tblIR.EscapeBackSlash()
		err                   error
	)

	selectedField := tblIR.SelectedField()
	// if has generated column
	if selectedField != "" {
		insertStatementPrefix = fmt.Sprintf("INSERT INTO %s %s VALUES\n",
			wrapBackTicks(tblIR.TableName()), selectedField)
	} else {
		insertStatementPrefix = fmt.Sprintf("INSERT INTO %s VALUES\n",
			wrapBackTicks(tblIR.TableName()))
	}

	for fileRowIter.HasNextSQLRowIter() {
		bf.WriteString(insertStatementPrefix)

		fileRowIter = fileRowIter.NextSQLRowIter()
		for fileRowIter.HasNext() {
			if err = fileRowIter.Decode(row); err != nil {
				log.Zap().Error("scanning from sql.Row failed", zap.Error(err))
				return err
			}

			row.WriteToBuffer(bf, escapeBackSlash)
			counter += 1

			if bf.Len() >= lengthLimit {
				wp.input <- bf.Bytes()
				bf.Reset()
			}

			fileRowIter.Next()
			if fileRowIter.HasNext() {
				bf.WriteString(",\n")
			} else {
				bf.WriteString(";\n")
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
		pool.Put(bf)
	}
	close(wp.input)
	<-wp.closed
	return wp.Error()
}

func write(writer io.StringWriter, str string) error {
	_, err := writer.WriteString(str)
	if err != nil {
		// str might be very long, only output the first 200 chars
		outputLength := len(str)
		if outputLength >= 200 {
			outputLength = 200
		}
		log.Zap().Error("writing failed",
			zap.String("string", str[:outputLength]),
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

func wrapBackTicks(identifier string) string {
	if !strings.HasPrefix(identifier, "`") && !strings.HasSuffix(identifier, "`") {
		return wrapStringWith(identifier, "`")
	}
	return identifier
}

func wrapStringWith(str string, wrapper string) string {
	return fmt.Sprintf("%s%s%s", wrapper, str, wrapper)
}
