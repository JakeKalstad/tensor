// Code generated by genlib2. DO NOT EDIT.

package tensor

import (
	"bytes"
	"encoding/binary"
	"encoding/csv"
	"encoding/gob"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/pkg/errors"
	"google.golang.org/protobuf/proto"
	"gorgonia.org/tensor/internal/serialization/fb"
	"gorgonia.org/tensor/internal/serialization/pb"
)

/* GOB SERIALIZATION */

// GobEncode implements gob.GobEncoder
func (t *Dense) GobEncode() (p []byte, err error) {
	var buf bytes.Buffer
	encoder := gob.NewEncoder(&buf)

	if err = encoder.Encode(t.Shape()); err != nil {
		return
	}

	if err = encoder.Encode(t.Strides()); err != nil {
		return
	}

	if err = encoder.Encode(t.AP.o); err != nil {
		return
	}

	if err = encoder.Encode(t.AP.Δ); err != nil {
		return
	}

	if err = encoder.Encode(t.mask); err != nil {
		return
	}

	data := t.Data()
	if err = encoder.Encode(&data); err != nil {
		return
	}

	return buf.Bytes(), err
}

// GobDecode implements gob.GobDecoder
func (t *Dense) GobDecode(p []byte) (err error) {
	buf := bytes.NewBuffer(p)
	decoder := gob.NewDecoder(buf)

	var shape Shape
	if err = decoder.Decode(&shape); err != nil {
		return
	}

	var strides []int
	if err = decoder.Decode(&strides); err != nil {
		return
	}

	var o DataOrder
	var tr Triangle
	if err = decoder.Decode(&o); err == nil {
		if err = decoder.Decode(&tr); err != nil {
			return
		}
	}

	t.AP.Init(shape, strides)
	t.AP.o = o
	t.AP.Δ = tr

	var mask []bool
	if err = decoder.Decode(&mask); err != nil {
		return
	}

	var data interface{}
	if err = decoder.Decode(&data); err != nil {
		return
	}

	t.fromSlice(data)
	t.addMask(mask)
	t.fix()
	if t.e == nil {
		t.e = StdEng{}
	}
	return t.sanity()
}

/* NPY SERIALIZATION */

var npyDescRE = regexp.MustCompile(`'descr':\s*'([^']*)'`)
var rowOrderRE = regexp.MustCompile(`'fortran_order':\s*(False|True)`)
var shapeRE = regexp.MustCompile(`'shape':\s*\(([^\(]*)\)`)

type binaryWriter struct {
	io.Writer
	err error
	seq int
}

func (w *binaryWriter) w(x interface{}) {
	if w.err != nil {
		return
	}

	w.err = binary.Write(w, binary.LittleEndian, x)
	w.seq++
}

func (w *binaryWriter) Err() error {
	if w.err == nil {
		return nil
	}
	return errors.Wrapf(w.err, "Sequence %d", w.seq)
}

type binaryReader struct {
	io.Reader
	err error
	seq int
}

func (r *binaryReader) Read(data interface{}) {
	if r.err != nil {
		return
	}
	r.err = binary.Read(r.Reader, binary.LittleEndian, data)
	r.seq++
}

func (r *binaryReader) Err() error {
	if r.err == nil {
		return nil
	}
	return errors.Wrapf(r.err, "Sequence %d", r.seq)
}

// WriteNpy writes the *Tensor as a numpy compatible serialized file.
//
// The format is very well documented here:
// http://docs.scipy.org/doc/numpy/neps/npy-format.html
//
// Gorgonia specifically uses Version 1.0, as 65535 bytes should be more than enough for the headers.
// The values are written in little endian order, because let's face it -
// 90% of the world's computers are running on x86+ processors.
//
// This method does not close the writer. Closing (if needed) is deferred to the caller
// If tensor is masked, invalid values are replaced by the default fill value.
func (t *Dense) WriteNpy(w io.Writer) (err error) {
	var npdt string
	if npdt, err = t.t.numpyDtype(); err != nil {
		return
	}

	var header string
	if t.Dims() == 1 {
		// when t is a 1D vector, numpy expects "(N,)" instead of "(N)" which t.Shape() returns.
		header = "{'descr': '<%v', 'fortran_order': False, 'shape': (%d,)}"
		header = fmt.Sprintf(header, npdt, t.Shape()[0])
	} else {
		header = "{'descr': '<%v', 'fortran_order': False, 'shape': %v}"
		header = fmt.Sprintf(header, npdt, t.Shape())
	}
	padding := 16 - ((10 + len(header)) % 16)
	if padding > 0 {
		header = header + strings.Repeat(" ", padding)
	}
	bw := binaryWriter{Writer: w}
	bw.Write([]byte("\x93NUMPY")) // stupid magic
	bw.w(byte(1))                 // major version
	bw.w(byte(0))                 // minor version
	bw.w(uint16(len(header)))     // 4 bytes to denote header length
	if err = bw.Err(); err != nil {
		return err
	}
	bw.Write([]byte(header))

	bw.seq = 0
	if t.IsMasked() {
		fillval := t.FillValue()
		it := FlatMaskedIteratorFromDense(t)
		for i, err := it.Next(); err == nil; i, err = it.Next() {
			if t.mask[i] {
				bw.w(fillval)
			} else {
				bw.w(t.Get(i))
			}
		}
	} else {
		for i := 0; i < t.len(); i++ {
			bw.w(t.Get(i))
		}
	}

	return bw.Err()
}

// ReadNpy reads NumPy formatted files into a *Dense
func (t *Dense) ReadNpy(r io.Reader) (err error) {
	br := binaryReader{Reader: r}
	var magic [6]byte
	if br.Read(magic[:]); string(magic[:]) != "\x93NUMPY" {
		return errors.Errorf("Not a numpy file. Got %q as the magic number instead", string(magic[:]))
	}

	var version, minor byte
	if br.Read(&version); version != 1 {
		return errors.New("Only verion 1.0 of numpy's serialization format is currently supported (65535 bytes ought to be enough for a header)")
	}

	if br.Read(&minor); minor != 0 {
		return errors.New("Only verion 1.0 of numpy's serialization format is currently supported (65535 bytes ought to be enough for a header)")
	}

	var headerLen uint16
	br.Read(&headerLen)
	header := make([]byte, int(headerLen))
	br.Read(header)
	if err = br.Err(); err != nil {
		return
	}

	// extract stuff from header
	var match [][]byte
	if match = npyDescRE.FindSubmatch(header); match == nil {
		return errors.New("No dtype information in npy file")
	}

	// TODO: check for endianness. For now we assume everything is little endian
	if t.t, err = fromNumpyDtype(string(match[1][1:])); err != nil {
		return
	}

	if match = rowOrderRE.FindSubmatch(header); match == nil {
		return errors.New("No Row Order information found in the numpy file")
	}
	if string(match[1]) != "False" {
		return errors.New("Cannot yet read from Fortran Ordered Numpy files")
	}

	if match = shapeRE.FindSubmatch(header); match == nil {
		return errors.New("No shape information found in npy file")
	}
	sizesStr := strings.Split(string(match[1]), ",")

	var shape Shape
	for _, s := range sizesStr {
		s = strings.Trim(s, " ")
		if len(s) == 0 {
			break
		}
		var size int
		if size, err = strconv.Atoi(s); err != nil {
			return
		}
		shape = append(shape, size)
	}

	size := shape.TotalSize()
	if t.e == nil {
		t.e = StdEng{}
	}
	t.makeArray(size)

	switch t.t.Kind() {
	case reflect.Int:
		data := t.Ints()
		for i := 0; i < size; i++ {
			br.Read(&data[i])
		}
	case reflect.Int8:
		data := t.Int8s()
		for i := 0; i < size; i++ {
			br.Read(&data[i])
		}
	case reflect.Int16:
		data := t.Int16s()
		for i := 0; i < size; i++ {
			br.Read(&data[i])
		}
	case reflect.Int32:
		data := t.Int32s()
		for i := 0; i < size; i++ {
			br.Read(&data[i])
		}
	case reflect.Int64:
		data := t.Int64s()
		for i := 0; i < size; i++ {
			br.Read(&data[i])
		}
	case reflect.Uint:
		data := t.Uints()
		for i := 0; i < size; i++ {
			br.Read(&data[i])
		}
	case reflect.Uint8:
		data := t.Uint8s()
		for i := 0; i < size; i++ {
			br.Read(&data[i])
		}
	case reflect.Uint16:
		data := t.Uint16s()
		for i := 0; i < size; i++ {
			br.Read(&data[i])
		}
	case reflect.Uint32:
		data := t.Uint32s()
		for i := 0; i < size; i++ {
			br.Read(&data[i])
		}
	case reflect.Uint64:
		data := t.Uint64s()
		for i := 0; i < size; i++ {
			br.Read(&data[i])
		}
	case reflect.Float32:
		data := t.Float32s()
		for i := 0; i < size; i++ {
			br.Read(&data[i])
		}
	case reflect.Float64:
		data := t.Float64s()
		for i := 0; i < size; i++ {
			br.Read(&data[i])
		}
	case reflect.Complex64:
		data := t.Complex64s()
		for i := 0; i < size; i++ {
			br.Read(&data[i])
		}
	case reflect.Complex128:
		data := t.Complex128s()
		for i := 0; i < size; i++ {
			br.Read(&data[i])
		}
	}
	if err = br.Err(); err != nil {
		return err
	}

	t.AP.zeroWithDims(len(shape))
	t.setShape(shape...)
	t.fix()
	return t.sanity()
}

/* CSV SERIALIZATION */

// WriteCSV writes the *Dense to a CSV. It accepts an optional string formatting ("%v", "%f", etc...), which controls what is written to the CSV.
// If tensor is masked, invalid values are replaced by the default fill value.
func (t *Dense) WriteCSV(w io.Writer, formats ...string) (err error) {
	// checks:
	if !t.IsMatrix() {
		// error
		err = errors.Errorf("Cannot write *Dense to CSV. Expected number of dimensions: <=2, T has got %d dimensions (Shape: %v)", t.Dims(), t.Shape())
		return
	}
	format := "%v"
	if len(formats) > 0 {
		format = formats[0]
	}

	cw := csv.NewWriter(w)
	it := IteratorFromDense(t)
	coord := it.Coord()

	// rows := t.Shape()[0]
	cols := t.Shape()[1]
	record := make([]string, 0, cols)
	var i, k, lastCol int
	isMasked := t.IsMasked()
	fillval := t.FillValue()
	fillstr := fmt.Sprintf(format, fillval)
	for i, err = it.Next(); err == nil; i, err = it.Next() {
		record = append(record, fmt.Sprintf(format, t.Get(i)))
		if isMasked {
			if t.mask[i] {
				record[k] = fillstr
			}
			k++
		}
		if lastCol == cols-1 {
			if err = cw.Write(record); err != nil {
				// TODO: wrap errors
				return
			}
			cw.Flush()
			record = record[:0]
		}

		// cleanup
		switch {
		case t.IsRowVec():
			// lastRow = coord[len(coord)-2]
			lastCol = coord[len(coord)-1]
		case t.IsColVec():
			// lastRow = coord[len(coord)-1]
			lastCol = coord[len(coord)-2]
		case t.IsVector():
			lastCol = coord[len(coord)-1]
		default:
			// lastRow = coord[len(coord)-2]
			lastCol = coord[len(coord)-1]
		}
	}
	return nil
}

// convFromStrs converts a []string to a slice of the Dtype provided. It takes a provided backing slice.
// If into is nil, then a backing slice will be created.
func convFromStrs(to Dtype, record []string, into interface{}) (interface{}, error) {
	var err error
	switch to.Kind() {
	case reflect.Int:
		retVal := make([]int, len(record))
		var backing []int
		if into == nil {
			backing = make([]int, 0, len(record))
		} else {
			backing = into.([]int)
		}

		for i, v := range record {
			var i64 int64
			if i64, err = strconv.ParseInt(v, 10, 0); err != nil {
				return nil, err
			}
			retVal[i] = int(i64)
		}
		backing = append(backing, retVal...)
		return backing, nil
	case reflect.Int8:
		retVal := make([]int8, len(record))
		var backing []int8
		if into == nil {
			backing = make([]int8, 0, len(record))
		} else {
			backing = into.([]int8)
		}

		for i, v := range record {
			var i64 int64
			if i64, err = strconv.ParseInt(v, 10, 8); err != nil {
				return nil, err
			}
			retVal[i] = int8(i64)
		}
		backing = append(backing, retVal...)
		return backing, nil
	case reflect.Int16:
		retVal := make([]int16, len(record))
		var backing []int16
		if into == nil {
			backing = make([]int16, 0, len(record))
		} else {
			backing = into.([]int16)
		}

		for i, v := range record {
			var i64 int64
			if i64, err = strconv.ParseInt(v, 10, 16); err != nil {
				return nil, err
			}
			retVal[i] = int16(i64)
		}
		backing = append(backing, retVal...)
		return backing, nil
	case reflect.Int32:
		retVal := make([]int32, len(record))
		var backing []int32
		if into == nil {
			backing = make([]int32, 0, len(record))
		} else {
			backing = into.([]int32)
		}

		for i, v := range record {
			var i64 int64
			if i64, err = strconv.ParseInt(v, 10, 32); err != nil {
				return nil, err
			}
			retVal[i] = int32(i64)
		}
		backing = append(backing, retVal...)
		return backing, nil
	case reflect.Int64:
		retVal := make([]int64, len(record))
		var backing []int64
		if into == nil {
			backing = make([]int64, 0, len(record))
		} else {
			backing = into.([]int64)
		}

		for i, v := range record {
			var i64 int64
			if i64, err = strconv.ParseInt(v, 10, 64); err != nil {
				return nil, err
			}
			retVal[i] = int64(i64)
		}
		backing = append(backing, retVal...)
		return backing, nil
	case reflect.Uint:
		retVal := make([]uint, len(record))
		var backing []uint
		if into == nil {
			backing = make([]uint, 0, len(record))
		} else {
			backing = into.([]uint)
		}

		for i, v := range record {
			var u uint64
			if u, err = strconv.ParseUint(v, 10, 0); err != nil {
				return nil, err
			}
			retVal[i] = uint(u)
		}
		backing = append(backing, retVal...)
		return backing, nil
	case reflect.Uint8:
		retVal := make([]uint8, len(record))
		var backing []uint8
		if into == nil {
			backing = make([]uint8, 0, len(record))
		} else {
			backing = into.([]uint8)
		}

		for i, v := range record {
			var u uint64
			if u, err = strconv.ParseUint(v, 10, 8); err != nil {
				return nil, err
			}
			retVal[i] = uint8(u)
		}
		backing = append(backing, retVal...)
		return backing, nil
	case reflect.Uint16:
		retVal := make([]uint16, len(record))
		var backing []uint16
		if into == nil {
			backing = make([]uint16, 0, len(record))
		} else {
			backing = into.([]uint16)
		}

		for i, v := range record {
			var u uint64
			if u, err = strconv.ParseUint(v, 10, 16); err != nil {
				return nil, err
			}
			retVal[i] = uint16(u)
		}
		backing = append(backing, retVal...)
		return backing, nil
	case reflect.Uint32:
		retVal := make([]uint32, len(record))
		var backing []uint32
		if into == nil {
			backing = make([]uint32, 0, len(record))
		} else {
			backing = into.([]uint32)
		}

		for i, v := range record {
			var u uint64
			if u, err = strconv.ParseUint(v, 10, 32); err != nil {
				return nil, err
			}
			retVal[i] = uint32(u)
		}
		backing = append(backing, retVal...)
		return backing, nil
	case reflect.Uint64:
		retVal := make([]uint64, len(record))
		var backing []uint64
		if into == nil {
			backing = make([]uint64, 0, len(record))
		} else {
			backing = into.([]uint64)
		}

		for i, v := range record {
			var u uint64
			if u, err = strconv.ParseUint(v, 10, 64); err != nil {
				return nil, err
			}
			retVal[i] = uint64(u)
		}
		backing = append(backing, retVal...)
		return backing, nil
	case reflect.Float32:
		retVal := make([]float32, len(record))
		var backing []float32
		if into == nil {
			backing = make([]float32, 0, len(record))
		} else {
			backing = into.([]float32)
		}

		for i, v := range record {
			var f float64
			if f, err = strconv.ParseFloat(v, 32); err != nil {
				return nil, err
			}
			retVal[i] = float32(f)
		}
		backing = append(backing, retVal...)
		return backing, nil
	case reflect.Float64:
		retVal := make([]float64, len(record))
		var backing []float64
		if into == nil {
			backing = make([]float64, 0, len(record))
		} else {
			backing = into.([]float64)
		}

		for i, v := range record {
			if retVal[i], err = strconv.ParseFloat(v, 64); err != nil {
				return nil, err
			}
		}
		backing = append(backing, retVal...)
		return backing, nil
	case reflect.String:
		var backing []string
		if into == nil {
			backing = make([]string, 0, len(record))
		} else {
			backing = into.([]string)
		}
		backing = append(backing, record...)
		return backing, nil
	default:
		return nil, errors.Errorf(methodNYI, "convFromStrs", to)
	}
}

// ReadCSV reads a CSV into a *Dense. It will override the underlying data.
//
// BUG(chewxy): reading CSV doesn't handle CSVs with different columns per row yet.
func (t *Dense) ReadCSV(r io.Reader, opts ...FuncOpt) (err error) {
	fo := ParseFuncOpts(opts...)
	as := fo.As()
	if as.Type == nil {
		as = Float64
	}

	cr := csv.NewReader(r)

	var record []string
	var rows, cols int
	var backing interface{}
	for {
		record, err = cr.Read()
		if err == io.EOF {
			break
		} else if err != nil {
			return
		}
		if backing, err = convFromStrs(as, record, backing); err != nil {
			return
		}
		cols = len(record)
		rows++
	}
	t.fromSlice(backing)
	t.AP.zero()
	t.AP.SetShape(rows, cols)
	return nil
	return errors.Errorf("not yet handled")
}

/* FB SERIALIZATION */

// FBEncode encodes to a byte slice using flatbuffers.
//
// Only natively accessible data can be encided
func (t *Dense) FBEncode() ([]byte, error) {
	builder := flatbuffers.NewBuilder(1024)

	fb.DenseStartShapeVector(builder, len(t.shape))
	for i := len(t.shape) - 1; i >= 0; i-- {
		builder.PrependInt32(int32(t.shape[i]))
	}
	shape := builder.EndVector(len(t.shape))

	fb.DenseStartStridesVector(builder, len(t.strides))
	for i := len(t.strides) - 1; i >= 0; i-- {
		builder.PrependInt32(int32(t.strides[i]))
	}
	strides := builder.EndVector(len(t.strides))

	var o uint32
	switch {
	case t.o.IsRowMajor() && t.o.IsContiguous():
		o = 0
	case t.o.IsRowMajor() && !t.o.IsContiguous():
		o = 1
	case t.o.IsColMajor() && t.o.IsContiguous():
		o = 2
	case t.o.IsColMajor() && !t.o.IsContiguous():
		o = 3
	}

	var triangle int32
	switch t.Δ {
	case NotTriangle:
		triangle = fb.TriangleNOT_TRIANGLE
	case Upper:
		triangle = fb.TriangleUPPER
	case Lower:
		triangle = fb.TriangleLOWER
	case Symmetric:
		triangle = fb.TriangleSYMMETRIC
	}

	dt := builder.CreateString(t.Dtype().String())
	data := t.byteSlice()

	fb.DenseStartDataVector(builder, len(data))
	for i := len(data) - 1; i >= 0; i-- {
		builder.PrependUint8(data[i])
	}
	databyte := builder.EndVector(len(data))

	fb.DenseStart(builder)
	fb.DenseAddShape(builder, shape)
	fb.DenseAddStrides(builder, strides)
	fb.DenseAddO(builder, o)
	fb.DenseAddT(builder, triangle)
	fb.DenseAddType(builder, dt)
	fb.DenseAddData(builder, databyte)
	serialized := fb.DenseEnd(builder)
	builder.Finish(serialized)

	return builder.FinishedBytes(), nil
}

// FBDecode decodes a byteslice from a flatbuffer table into a *Dense
func (t *Dense) FBDecode(buf []byte) error {
	serialized := fb.GetRootAsDense(buf, 0)

	o := serialized.O()
	switch o {
	case 0:
		t.o = 0
	case 1:
		t.o = MakeDataOrder(NonContiguous)
	case 2:
		t.o = MakeDataOrder(ColMajor)
	case 3:
		t.o = MakeDataOrder(ColMajor, NonContiguous)
	}

	tri := serialized.T()
	switch tri {
	case fb.TriangleNOT_TRIANGLE:
		t.Δ = NotTriangle
	case fb.TriangleUPPER:
		t.Δ = Upper
	case fb.TriangleLOWER:
		t.Δ = Lower
	case fb.TriangleSYMMETRIC:
		t.Δ = Symmetric
	}

	t.shape = Shape(BorrowInts(serialized.ShapeLength()))
	for i := 0; i < serialized.ShapeLength(); i++ {
		t.shape[i] = int(int32(serialized.Shape(i)))
	}

	t.strides = BorrowInts(serialized.StridesLength())
	for i := 0; i < serialized.ShapeLength(); i++ {
		t.strides[i] = int(serialized.Strides(i))
	}
	typ := string(serialized.Type())
	for _, dt := range allTypes.set {
		if dt.String() == typ {
			t.t = dt
			break
		}
	}

	if t.e == nil {
		t.e = StdEng{}
	}
	t.makeArray(t.shape.TotalSize())

	// allocated data. Now time to actually copy over the data
	db := t.byteSlice()
	copy(db, serialized.DataBytes())
	t.fix()
	return t.sanity()
}

/* PB SERIALIZATION */

// PBEncode encodes the Dense into a protobuf byte slice.
func (t *Dense) PBEncode() ([]byte, error) {
	var toSerialize pb.Dense
	toSerialize.Shape = make([]int32, len(t.shape))
	for i, v := range t.shape {
		toSerialize.Shape[i] = int32(v)
	}
	toSerialize.Strides = make([]int32, len(t.strides))
	for i, v := range t.strides {
		toSerialize.Strides[i] = int32(v)
	}

	toSerialize.T = pb.Triangle(t.Δ)
	toSerialize.Type = t.t.String()
	data := t.byteSlice()
	toSerialize.Data = make([]byte, len(data))
	copy(toSerialize.Data, data)
	
	return proto.Marshal(&toSerialize)
}

// PBDecode unmarshalls a protobuf byteslice into a *Dense.
func (t *Dense) PBDecode(buf []byte) error {
	toSerialize := pb.Dense{}
	if err := proto.Unmarshal(buf, &toSerialize); err != nil {
		return err
	}
	t.shape = make(Shape, len(toSerialize.Shape))
	for i, v := range toSerialize.Shape {
		t.shape[i] = int(v)
	}
	t.strides = make([]int, len(toSerialize.Strides))
	for i, v := range toSerialize.Strides {
		t.strides[i] = int(v)
	}
 
	t.Δ = Triangle(toSerialize.T)
	typ := string(toSerialize.Type)
	for _, dt := range allTypes.set {
		if dt.String() == typ {
			t.t = dt
			break
		}
	}

	if t.e == nil {
		t.e = StdEng{}
	}
	t.makeArray(t.shape.TotalSize())

	// allocated data. Now time to actually copy over the data
	db := t.byteSlice()
	copy(db, toSerialize.Data)
	return t.sanity()
}
