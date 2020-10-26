package spanstore

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gogo/protobuf/proto"
	"go.uber.org/zap"

	"github.com/jaegertracing/jaeger/model"
)

type Encoding string

const (
	// EncodingJSON is used for spans encoded as JSON.
	EncodingJSON Encoding = "json"
	// EncodingProto is used for spans encoded as Protobuf.
	EncodingProto Encoding = "protobuf"
)

// SpanWriter for writing spans to ClickHouse
type SpanWriter struct {
	logger     *zap.Logger
	db         *sql.DB
	indexTable string
	spansTable string
	encoding   Encoding
	delay      time.Duration
	size       int
	spans      chan *model.Span
	finish     chan bool
	done       sync.WaitGroup
}

// NewSpanWriter returns a SpanWriter for the database
func NewSpanWriter(logger *zap.Logger, db *sql.DB, indexTable string, spansTable string, encoding Encoding, delay time.Duration, size int) *SpanWriter {
	writer := &SpanWriter{
		logger:     logger,
		db:         db,
		indexTable: indexTable,
		spansTable: spansTable,
		encoding:   encoding,
		delay:      delay,
		size:       size,
		spans:      make(chan *model.Span, size),
		finish:     make(chan bool),
	}

	go writer.backgroundWriter()

	return writer
}

func (w *SpanWriter) backgroundWriter() {
	batch := make([]*model.Span, 0, w.size)

	timer := time.After(w.delay)
	last := time.Now()

	for {
		w.done.Add(1)

		flush := false
		finish := false

		select {
		case span := <-w.spans:
			batch = append(batch, span)
			flush = len(batch) == cap(batch)
		case <-timer:
			timer = time.After(w.delay)
			flush = time.Since(last) > w.delay && len(batch) > 0
		case <-w.finish:
			finish = true
			flush = len(batch) > 0
		}

		if flush {
			if err := w.writeBatch(batch); err != nil {
				w.logger.Error("Could not write a batch of spans", zap.Error(err))
			}

			batch = make([]*model.Span, 0, w.size)
			last = time.Now()
		}

		w.done.Done()

		if finish {
			break
		}
	}
}

func (w *SpanWriter) writeBatch(batch []*model.Span) error {
	if err := w.writeModelBatch(batch); err != nil {
		return err
	}

	if w.indexTable != "" {
		if err := w.writeIndexBatch(batch); err != nil {
			return err
		}
	}

	return nil
}

func (w *SpanWriter) writeModelBatch(batch []*model.Span) error {
	tx, err := w.db.Begin()
	if err != nil {
		return err
	}

	commited := false

	defer func() {
		if !commited {
			// Clickhouse does not support real rollback
			_ = tx.Rollback()
		}
	}()

	statement, err := tx.Prepare(fmt.Sprintf("INSERT INTO %s (timestamp, traceID, model) VALUES (?, ?, ?)", w.spansTable))
	if err != nil {
		return nil
	}

	defer statement.Close()

	for _, span := range batch {
		var serialized []byte

		if w.encoding == EncodingJSON {
			serialized, err = json.Marshal(span)
		} else {
			serialized, err = proto.Marshal(span)
		}

		if err != nil {
			return err
		}

		_, err = statement.Exec(span.StartTime, span.TraceID.String(), serialized)
		if err != nil {
			return err
		}
	}

	commited = true

	return tx.Commit()
}

func (w *SpanWriter) writeIndexBatch(batch []*model.Span) error {
	tx, err := w.db.Begin()
	if err != nil {
		return err
	}

	commited := false

	defer func() {
		if !commited {
			// Clickhouse does not support real rollback
			_ = tx.Rollback()
		}
	}()

	statement, err := tx.Prepare(fmt.Sprintf("INSERT INTO %s (timestamp, traceID, service, operation, durationUs, tags) VALUES (?, ?, ?, ?, ?, ?)", w.indexTable))
	if err != nil {
		return err
	}

	defer statement.Close()

	for _, span := range batch {
		_, err = statement.Exec(
			span.StartTime,
			span.TraceID.String(),
			span.Process.ServiceName,
			span.OperationName,
			span.Duration.Microseconds(),
			uniqueTagsForSpan(span),
		)
		if err != nil {
			return err
		}
	}

	commited = true

	return tx.Commit()
}

// WriteSpan writes the encoded span
func (w *SpanWriter) WriteSpan(span *model.Span) error {
	w.spans <- span
	return nil
}

// Close Implements io.Closer and closes the underlying storage
func (w *SpanWriter) Close() error {
	w.finish <- true
	w.done.Wait()
	return nil
}

func uniqueTagsForSpan(span *model.Span) []string {
	uniqueTags := make(map[string]struct{}, len(span.Tags)+len(span.Process.Tags))

	buf := &strings.Builder{}

	for _, kv := range span.Tags {
		uniqueTags[tagString(buf, &kv)] = struct{}{}
	}

	for _, kv := range span.Process.Tags {
		uniqueTags[tagString(buf, &kv)] = struct{}{}
	}

	for _, event := range span.Logs {
		for _, kv := range event.Fields {
			uniqueTags[tagString(buf, &kv)] = struct{}{}
		}
	}

	tags := make([]string, 0, len(uniqueTags))

	for kv := range uniqueTags {
		tags = append(tags, kv)
	}

	sort.Strings(tags)

	return tags
}

func tagString(buf *strings.Builder, kv *model.KeyValue) string {
	buf.Reset()

	buf.WriteString(kv.Key)
	buf.WriteByte('=')
	buf.WriteString(kv.AsString())

	return buf.String()
}
