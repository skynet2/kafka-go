package protocol

import (
	"errors"
	"io"
	"time"
)

// RecordReader is an interface representing a sequence of records. Record sets
// are used in both produce and fetch requests to represent the sequence of
// records that are sent to or receive from kafka brokers.
//
// RecordSet values are not safe to use concurrently from multiple goroutines.
type RecordReader interface {
	// Returns the next record in the set, or io.EOF if the end of the sequence
	// has been reached.
	//
	// The returned Record is guaranteed to be valid until the next call to
	// ReadRecord. If the program needs to retain the Record value it must make
	// a copy.
	ReadRecord() (*Record, error)
}

// NewRecordReader constructs a reader exposing the records passed as arguments.
func NewRecordReader(records ...Record) RecordReader {
	switch len(records) {
	case 0:
		return emptyRecordReader{}
	default:
		r := &recordReader{records: make([]Record, len(records))}
		copy(r.records, records)
		return r
	}
}

// MultiRecordReader merges multiple record batches into one.
func MultiRecordReader(batches ...RecordReader) RecordReader {
	switch len(batches) {
	case 0:
		return emptyRecordReader{}
	case 1:
		return batches[0]
	default:
		m := &multiRecordReader{batches: make([]RecordReader, len(batches))}
		copy(m.batches, batches)
		return m
	}
}

// ResetRecordReader is a helper function to reset record batches that have a
// `Reset()` method.
func ResetRecordReader(rb RecordReader) error {
	if r, _ := rb.(resetter); r != nil {
		return r.Reset()
	}
	return ErrNoReset
}

type resetter interface {
	Reset() error
}

func forEachRecord(r RecordReader, f func(int, *Record) error) error {
	for i := 0; ; i++ {
		rec, err := r.ReadRecord()

		if err != nil {
			if errors.Is(err, io.EOF) {
				err = nil
			}
			return err
		}

		if err := handleRecord(i, rec, f); err != nil {
			return err
		}
	}
}

func handleRecord(i int, r *Record, f func(int, *Record) error) error {
	if r.Key != nil {
		defer r.Key.Close()
	}
	if r.Value != nil {
		defer r.Value.Close()
	}
	if r.Offset == 0 {
		r.Offset = int64(i)
	}
	return f(i, r)
}

type recordReader struct {
	records []Record
	index   int
}

func (r *recordReader) ReadRecord() (*Record, error) {
	if i := r.index; i >= 0 && i < len(r.records) {
		r.index++
		return &r.records[i], nil
	}
	return nil, io.EOF
}

func (r *recordReader) Reset() error {
	r.index = 0
	for i := range r.records {
		r := &r.records[i]
		resetBytes(r.Key)
		resetBytes(r.Value)
	}
	return nil
}

type multiRecordReader struct {
	batches []RecordReader
	index   int
}

func (m *multiRecordReader) ReadRecord() (*Record, error) {
	for {
		if m.index == len(m.batches) {
			return nil, io.EOF
		}
		r, err := m.batches[m.index].ReadRecord()
		if err == nil {
			return r, nil
		}
		if !errors.Is(err, io.EOF) {
			return nil, err
		}
		m.index++
	}
}

func (m *multiRecordReader) Reset() (err error) {
	m.index, err = resetRecordReaders(m.batches[:m.index])
	return
}

func concatRecordReader(head RecordReader, tail RecordReader) RecordReader {
	if head == nil {
		return tail
	}
	if m, _ := head.(*multiRecordReader); m != nil {
		m.batches = append(m.batches, tail)
		return m
	}
	return MultiRecordReader(head, tail)
}

type optimizedRecordReader struct {
	records []optimizedRecord
	index   int
	buffer  Record
}

func (r *optimizedRecordReader) Reset() error {
	for i := range r.records {
		resetBytes(r.records[i].key())
		resetBytes(r.records[i].value())
	}
	return nil
}

func (r *optimizedRecordReader) ReadRecord() (*Record, error) {
	if i := r.index; i >= 0 && i < len(r.records) {
		rec := &r.records[i]
		r.index++
		r.buffer = Record{
			Offset:  rec.offset,
			Time:    rec.time(),
			Key:     rec.key(),
			Value:   rec.value(),
			Headers: rec.headers,
		}
		return &r.buffer, nil
	}
	return nil, io.EOF
}

type optimizedRecord struct {
	offset    int64
	timestamp int64
	keyRef    pageRef
	valueRef  pageRef
	headers   []Header
}

func (r *optimizedRecord) unref() {
	r.keyRef.unref()
	r.valueRef.unref()
}

func (r *optimizedRecord) time() time.Time {
	return makeTime(r.timestamp)
}

func (r *optimizedRecord) key() Bytes {
	return makeBytes(&r.keyRef)
}

func (r *optimizedRecord) value() Bytes {
	return makeBytes(&r.valueRef)
}

type emptyRecordReader struct{}

func (emptyRecordReader) ReadRecord() (*Record, error) { return nil, io.EOF }

// ControlRecord represents a record read from a control batch.
type ControlRecord struct {
	Offset  int64
	Time    time.Time
	Version int16
	Type    int16
	Data    []byte
	Headers []Header
}

func ReadControlRecord(r *Record) (*ControlRecord, error) {
	k, err := ReadFull(r.Key)
	if err != nil {
		return nil, err
	}
	if k == nil {
		return nil, Error("invalid control record with nil key")
	}
	if len(k) != 4 {
		return nil, Errorf("invalid control record with key of size %d", len(k))
	}

	v, err := ReadFull(r.Value)
	if err != nil {
		return nil, err
	}

	c := &ControlRecord{
		Offset:  r.Offset,
		Time:    r.Time,
		Version: readInt16(k[:2]),
		Type:    readInt16(k[2:]),
		Data:    v,
		Headers: r.Headers,
	}

	return c, nil
}

func (cr *ControlRecord) Key() Bytes {
	k := make([]byte, 4)
	writeInt16(k[:2], cr.Version)
	writeInt16(k[2:], cr.Type)
	return NewBytes(k)
}

func (cr *ControlRecord) Value() Bytes {
	return NewBytes(cr.Data)
}

func (cr *ControlRecord) Record() Record {
	return Record{
		Offset:  cr.Offset,
		Time:    cr.Time,
		Key:     cr.Key(),
		Value:   cr.Value(),
		Headers: cr.Headers,
	}
}

// ControlBatch is an implementation of the RecordReader interface representing
// control batches returned by kafka brokers.
type ControlBatch struct {
	Attributes           Attributes
	PartitionLeaderEpoch int32
	BaseOffset           int64
	ProducerID           int64
	ProducerEpoch        int16
	BaseSequence         int32
	Records              RecordReader
}

// NewControlBatch constructs a control batch from the list of records passed as
// arguments.
func NewControlBatch(records ...ControlRecord) *ControlBatch {
	rawRecords := make([]Record, len(records))
	for i, cr := range records {
		rawRecords[i] = cr.Record()
	}
	return &ControlBatch{
		Records: NewRecordReader(rawRecords...),
	}
}

func (c *ControlBatch) ReadRecord() (*Record, error) {
	return c.Records.ReadRecord()
}

func (c *ControlBatch) ReadControlRecord() (*ControlRecord, error) {
	r, err := c.ReadRecord()
	if err != nil {
		return nil, err
	}
	if r.Key != nil {
		defer r.Key.Close()
	}
	if r.Value != nil {
		defer r.Value.Close()
	}
	return ReadControlRecord(r)
}

func (c *ControlBatch) Reset() error {
	return ResetRecordReader(c.Records)
}

func (c *ControlBatch) Offset() int64 {
	return c.BaseOffset
}

func (c *ControlBatch) Version() int {
	return 2
}

// RecordBatch is an implementation of the RecordReader interface representing
// regular record batches (v2).
type RecordBatch struct {
	Attributes           Attributes
	PartitionLeaderEpoch int32
	BaseOffset           int64
	ProducerID           int64
	ProducerEpoch        int16
	BaseSequence         int32
	Records              RecordReader
}

func (r *RecordBatch) ReadRecord() (*Record, error) {
	return r.Records.ReadRecord()
}

func (r *RecordBatch) Reset() error {
	return ResetRecordReader(r.Records)
}

func (r *RecordBatch) Offset() int64 {
	return r.BaseOffset
}

func (r *RecordBatch) Version() int {
	return 2
}

// MessageSet is an implementation of the RecordReader interface representing
// regular message sets (v1).
type MessageSet struct {
	Attributes Attributes
	BaseOffset int64
	Records    RecordReader
}

func (m *MessageSet) ReadRecord() (*Record, error) {
	return m.Records.ReadRecord()
}

func (m *MessageSet) Reset() error {
	return ResetRecordReader(m.Records)
}

func (m *MessageSet) Offset() int64 {
	return m.BaseOffset
}

func (m *MessageSet) Version() int {
	return 1
}

// RecordStream is an implementation of the RecordReader interface which
// combines multiple underlying RecordReader and only expose records that
// are not from control batches.
type RecordStream struct {
	Records []RecordReader
	index   int
}

func (s *RecordStream) ReadRecord() (*Record, error) {
	for {
		if s.index < 0 || s.index >= len(s.Records) {
			return nil, io.EOF
		}

		if _, isControl := s.Records[s.index].(*ControlBatch); isControl {
			s.index++
			continue
		}

		r, err := s.Records[s.index].ReadRecord()
		if err != nil {
			if errors.Is(err, io.EOF) {
				s.index++
				continue
			}
		}

		return r, err
	}
}

func (s *RecordStream) Reset() (err error) {
	s.index, err = resetRecordReaders(s.Records[:s.index])
	return
}

type message struct {
	Record Record
	read   bool
}

func (m *message) ReadRecord() (*Record, error) {
	if m.read {
		return nil, io.EOF
	}
	m.read = true
	return &m.Record, nil
}

func (m *message) Reset() error {
	m.read = false
	return nil
}

func (m *message) Offset() int64 {
	return m.Record.Offset
}

func (m *message) Version() int {
	return 1
}

func resetRecordReaders(records []RecordReader) (int, error) {
	i := len(records) - 1

	for i >= 0 {
		if err := ResetRecordReader(records[i]); err != nil {
			return i + 1, err
		}
		i--
	}

	return i + 1, nil
}
