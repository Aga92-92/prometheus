// Copyright 2017 The Prometheus Authors
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

package remote

import (
	"bytes"
	"fmt"
	"sync"
	"testing"

	"github.com/go-kit/log"
	"github.com/gogo/protobuf/proto"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/prompb"
	writev2 "github.com/prometheus/prometheus/prompb/io/prometheus/write/v2"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/tsdbutil"
	"github.com/prometheus/prometheus/util/annotations"
)

var testHistogram = histogram.Histogram{
	Schema:          2,
	ZeroThreshold:   1e-128,
	ZeroCount:       0,
	Count:           0,
	Sum:             20,
	PositiveSpans:   []histogram.Span{{Offset: 0, Length: 1}},
	PositiveBuckets: []int64{1},
	NegativeSpans:   []histogram.Span{{Offset: 0, Length: 1}},
	NegativeBuckets: []int64{-1},
}

var writeRequestFixture = &prompb.WriteRequest{
	Timeseries: []prompb.TimeSeries{
		{
			Labels: []prompb.Label{
				{Name: "__name__", Value: "test_metric1"},
				{Name: "b", Value: "c"},
				{Name: "baz", Value: "qux"},
				{Name: "d", Value: "e"},
				{Name: "foo", Value: "bar"},
			},
			Samples:    []prompb.Sample{{Value: 1, Timestamp: 0}},
			Exemplars:  []prompb.Exemplar{{Labels: []prompb.Label{{Name: "f", Value: "g"}}, Value: 1, Timestamp: 0}},
			Histograms: []prompb.Histogram{prompb.FromIntHistogram(0, &testHistogram), prompb.FromFloatHistogram(1, testHistogram.ToFloat(nil))},
		},
		{
			Labels: []prompb.Label{
				{Name: "__name__", Value: "test_metric1"},
				{Name: "b", Value: "c"},
				{Name: "baz", Value: "qux"},
				{Name: "d", Value: "e"},
				{Name: "foo", Value: "bar"},
			},
			Samples:    []prompb.Sample{{Value: 2, Timestamp: 1}},
			Exemplars:  []prompb.Exemplar{{Labels: []prompb.Label{{Name: "h", Value: "i"}}, Value: 2, Timestamp: 1}},
			Histograms: []prompb.Histogram{prompb.FromIntHistogram(2, &testHistogram), prompb.FromFloatHistogram(3, testHistogram.ToFloat(nil))},
		},
	},
}

// writeV2RequestFixture represents the same request as writeRequestFixture, but using the v2 representation.
var writeV2RequestFixture = func() *writev2.Request {
	st := writev2.NewSymbolTable()
	b := labels.NewScratchBuilder(0)
	labelRefs := st.SymbolizeLabels(writeRequestFixture.Timeseries[0].ToLabels(&b, nil), nil)
	exemplar1LabelRefs := st.SymbolizeLabels(writeRequestFixture.Timeseries[0].Exemplars[0].ToExemplar(&b, nil).Labels, nil)
	exemplar2LabelRefs := st.SymbolizeLabels(writeRequestFixture.Timeseries[0].Exemplars[0].ToExemplar(&b, nil).Labels, nil)
	return &writev2.Request{
		Timeseries: []writev2.TimeSeries{
			{
				LabelsRefs: labelRefs,
				Samples:    []writev2.Sample{{Value: 1, Timestamp: 0}},
				Exemplars:  []writev2.Exemplar{{LabelsRefs: exemplar1LabelRefs, Value: 1, Timestamp: 0}},
				Histograms: []writev2.Histogram{writev2.FromIntHistogram(0, &testHistogram), writev2.FromFloatHistogram(1, testHistogram.ToFloat(nil))},
			},
			{
				LabelsRefs: labelRefs,
				Samples:    []writev2.Sample{{Value: 2, Timestamp: 1}},
				Exemplars:  []writev2.Exemplar{{LabelsRefs: exemplar2LabelRefs, Value: 2, Timestamp: 1}},
				Histograms: []writev2.Histogram{writev2.FromIntHistogram(2, &testHistogram), writev2.FromFloatHistogram(3, testHistogram.ToFloat(nil))},
			},
		},
		Symbols: st.Symbols(),
	}
}()

func TestValidateLabelsAndMetricName(t *testing.T) {
	tests := []struct {
		input       []prompb.Label
		expectedErr string
		description string
	}{
		{
			input: []prompb.Label{
				{Name: "__name__", Value: "name"},
				{Name: "labelName", Value: "labelValue"},
			},
			expectedErr: "",
			description: "regular labels",
		},
		{
			input: []prompb.Label{
				{Name: "__name__", Value: "name"},
				{Name: "_labelName", Value: "labelValue"},
			},
			expectedErr: "",
			description: "label name with _",
		},
		{
			input: []prompb.Label{
				{Name: "__name__", Value: "name"},
				{Name: "@labelName", Value: "labelValue"},
			},
			expectedErr: "invalid label name: @labelName",
			description: "label name with @",
		},
		{
			input: []prompb.Label{
				{Name: "__name__", Value: "name"},
				{Name: "123labelName", Value: "labelValue"},
			},
			expectedErr: "invalid label name: 123labelName",
			description: "label name starts with numbers",
		},
		{
			input: []prompb.Label{
				{Name: "__name__", Value: "name"},
				{Name: "", Value: "labelValue"},
			},
			expectedErr: "invalid label name: ",
			description: "label name is empty string",
		},
		{
			input: []prompb.Label{
				{Name: "__name__", Value: "name"},
				{Name: "labelName", Value: string([]byte{0xff})},
			},
			expectedErr: "invalid label value: " + string([]byte{0xff}),
			description: "label value is an invalid UTF-8 value",
		},
		{
			input: []prompb.Label{
				{Name: "__name__", Value: "@invalid_name"},
			},
			expectedErr: "invalid metric name: @invalid_name",
			description: "metric name starts with @",
		},
		{
			input: []prompb.Label{
				{Name: "__name__", Value: "name1"},
				{Name: "__name__", Value: "name2"},
			},
			expectedErr: "duplicate label with name: __name__",
			description: "duplicate label names",
		},
		{
			input: []prompb.Label{
				{Name: "label1", Value: "name"},
				{Name: "label2", Value: "name"},
			},
			expectedErr: "",
			description: "duplicate label values",
		},
		{
			input: []prompb.Label{
				{Name: "", Value: "name"},
				{Name: "label2", Value: "name"},
			},
			expectedErr: "invalid label name: ",
			description: "don't report as duplicate label name",
		},
	}

	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			err := validateLabelsAndMetricName(test.input)
			if test.expectedErr != "" {
				require.Error(t, err)
				require.Equal(t, test.expectedErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestConcreteSeriesSet(t *testing.T) {
	series1 := &concreteSeries{
		labels: labels.FromStrings("foo", "bar"),
		floats: []prompb.Sample{{Value: 1, Timestamp: 2}},
	}
	series2 := &concreteSeries{
		labels: labels.FromStrings("foo", "baz"),
		floats: []prompb.Sample{{Value: 3, Timestamp: 4}},
	}
	c := &concreteSeriesSet{
		series: []storage.Series{series1, series2},
	}
	require.True(t, c.Next(), "Expected Next() to be true.")
	require.Equal(t, series1, c.At(), "Unexpected series returned.")
	require.True(t, c.Next(), "Expected Next() to be true.")
	require.Equal(t, series2, c.At(), "Unexpected series returned.")
	require.False(t, c.Next(), "Expected Next() to be false.")
}

func TestConcreteSeriesClonesLabels(t *testing.T) {
	lbls := labels.FromStrings("a", "b", "c", "d")
	cs := concreteSeries{
		labels: lbls,
	}

	gotLabels := cs.Labels()
	require.Equal(t, lbls, gotLabels)

	gotLabels.CopyFrom(labels.FromStrings("a", "foo", "c", "foo"))

	gotLabels = cs.Labels()
	require.Equal(t, lbls, gotLabels)
}

func TestConcreteSeriesIterator_FloatSamples(t *testing.T) {
	series := &concreteSeries{
		labels: labels.FromStrings("foo", "bar"),
		floats: []prompb.Sample{
			{Value: 1, Timestamp: 1},
			{Value: 1.5, Timestamp: 1},
			{Value: 2, Timestamp: 2},
			{Value: 3, Timestamp: 3},
			{Value: 4, Timestamp: 4},
		},
	}
	it := series.Iterator(nil)

	// Seek to the first sample with ts=1.
	require.Equal(t, chunkenc.ValFloat, it.Seek(1))
	ts, v := it.At()
	require.Equal(t, int64(1), ts)
	require.Equal(t, 1., v)

	// Seek one further, next sample still has ts=1.
	require.Equal(t, chunkenc.ValFloat, it.Next())
	ts, v = it.At()
	require.Equal(t, int64(1), ts)
	require.Equal(t, 1.5, v)

	// Seek again to 1 and make sure we stay where we are.
	require.Equal(t, chunkenc.ValFloat, it.Seek(1))
	ts, v = it.At()
	require.Equal(t, int64(1), ts)
	require.Equal(t, 1.5, v)

	// Another seek.
	require.Equal(t, chunkenc.ValFloat, it.Seek(3))
	ts, v = it.At()
	require.Equal(t, int64(3), ts)
	require.Equal(t, 3., v)

	// And we don't go back.
	require.Equal(t, chunkenc.ValFloat, it.Seek(2))
	ts, v = it.At()
	require.Equal(t, int64(3), ts)
	require.Equal(t, 3., v)

	// Seek beyond the end.
	require.Equal(t, chunkenc.ValNone, it.Seek(5))
	// And we don't go back. (This exposes issue #10027.)
	require.Equal(t, chunkenc.ValNone, it.Seek(2))
}

func TestConcreteSeriesIterator_HistogramSamples(t *testing.T) {
	histograms := tsdbutil.GenerateTestHistograms(5)
	histProtos := make([]prompb.Histogram, len(histograms))
	for i, h := range histograms {
		// Results in ts sequence of 1, 1, 2, 3, 4.
		var ts int64
		if i == 0 {
			ts = 1
		} else {
			ts = int64(i)
		}
		histProtos[i] = prompb.FromIntHistogram(ts, h)
	}
	series := &concreteSeries{
		labels:     labels.FromStrings("foo", "bar"),
		histograms: histProtos,
	}
	it := series.Iterator(nil)

	// Seek to the first sample with ts=1.
	require.Equal(t, chunkenc.ValHistogram, it.Seek(1))
	ts, v := it.AtHistogram(nil)
	require.Equal(t, int64(1), ts)
	require.Equal(t, histograms[0], v)

	// Seek one further, next sample still has ts=1.
	require.Equal(t, chunkenc.ValHistogram, it.Next())
	ts, v = it.AtHistogram(nil)
	require.Equal(t, int64(1), ts)
	require.Equal(t, histograms[1], v)

	// Seek again to 1 and make sure we stay where we are.
	require.Equal(t, chunkenc.ValHistogram, it.Seek(1))
	ts, v = it.AtHistogram(nil)
	require.Equal(t, int64(1), ts)
	require.Equal(t, histograms[1], v)

	// Another seek.
	require.Equal(t, chunkenc.ValHistogram, it.Seek(3))
	ts, v = it.AtHistogram(nil)
	require.Equal(t, int64(3), ts)
	require.Equal(t, histograms[3], v)

	// And we don't go back.
	require.Equal(t, chunkenc.ValHistogram, it.Seek(2))
	ts, v = it.AtHistogram(nil)
	require.Equal(t, int64(3), ts)
	require.Equal(t, histograms[3], v)

	// Seek beyond the end.
	require.Equal(t, chunkenc.ValNone, it.Seek(5))
	// And we don't go back. (This exposes issue #10027.)
	require.Equal(t, chunkenc.ValNone, it.Seek(2))
}

func TestConcreteSeriesIterator_FloatAndHistogramSamples(t *testing.T) {
	// Series starts as histograms, then transitions to floats at ts=8 (with an overlap from ts=8 to ts=10), then
	// transitions back to histograms at ts=16.
	histograms := tsdbutil.GenerateTestHistograms(15)
	histProtos := make([]prompb.Histogram, len(histograms))
	for i, h := range histograms {
		if i < 10 {
			histProtos[i] = prompb.FromIntHistogram(int64(i+1), h)
		} else {
			histProtos[i] = prompb.FromIntHistogram(int64(i+6), h)
		}
	}
	series := &concreteSeries{
		labels: labels.FromStrings("foo", "bar"),
		floats: []prompb.Sample{
			{Value: 1, Timestamp: 8},
			{Value: 2, Timestamp: 9},
			{Value: 3, Timestamp: 10},
			{Value: 4, Timestamp: 11},
			{Value: 5, Timestamp: 12},
			{Value: 6, Timestamp: 13},
			{Value: 7, Timestamp: 14},
			{Value: 8, Timestamp: 15},
		},
		histograms: histProtos,
	}
	it := series.Iterator(nil)

	var (
		ts int64
		v  float64
		h  *histogram.Histogram
		fh *histogram.FloatHistogram
	)
	require.Equal(t, chunkenc.ValHistogram, it.Next())
	ts, h = it.AtHistogram(nil)
	require.Equal(t, int64(1), ts)
	require.Equal(t, histograms[0], h)

	require.Equal(t, chunkenc.ValHistogram, it.Next())
	ts, h = it.AtHistogram(nil)
	require.Equal(t, int64(2), ts)
	require.Equal(t, histograms[1], h)

	// Seek to the first float/histogram sample overlap at ts=8 (should prefer the float sample).
	require.Equal(t, chunkenc.ValFloat, it.Seek(8))
	ts, v = it.At()
	require.Equal(t, int64(8), ts)
	require.Equal(t, 1., v)

	// Attempting to seek backwards should do nothing.
	require.Equal(t, chunkenc.ValFloat, it.Seek(1))
	ts, v = it.At()
	require.Equal(t, int64(8), ts)
	require.Equal(t, 1., v)

	// Seeking to 8 again should also do nothing.
	require.Equal(t, chunkenc.ValFloat, it.Seek(8))
	ts, v = it.At()
	require.Equal(t, int64(8), ts)
	require.Equal(t, 1., v)

	// Again, should prefer the float sample.
	require.Equal(t, chunkenc.ValFloat, it.Next())
	ts, v = it.At()
	require.Equal(t, int64(9), ts)
	require.Equal(t, 2., v)

	// Seek to ts=11 where there are only float samples.
	require.Equal(t, chunkenc.ValFloat, it.Seek(11))
	ts, v = it.At()
	require.Equal(t, int64(11), ts)
	require.Equal(t, 4., v)

	// Seek to ts=15 right before the transition back to histogram samples.
	require.Equal(t, chunkenc.ValFloat, it.Seek(15))
	ts, v = it.At()
	require.Equal(t, int64(15), ts)
	require.Equal(t, 8., v)

	require.Equal(t, chunkenc.ValHistogram, it.Next())
	ts, h = it.AtHistogram(nil)
	require.Equal(t, int64(16), ts)
	require.Equal(t, histograms[10], h)

	// Getting a float histogram from an int histogram works.
	require.Equal(t, chunkenc.ValHistogram, it.Next())
	ts, fh = it.AtFloatHistogram(nil)
	require.Equal(t, int64(17), ts)
	expected := prompb.FromIntHistogram(int64(17), histograms[11]).ToFloatHistogram()
	require.Equal(t, expected, fh)

	// Keep calling Next() until the end.
	for i := 0; i < 3; i++ {
		require.Equal(t, chunkenc.ValHistogram, it.Next())
	}

	// The iterator is exhausted.
	require.Equal(t, chunkenc.ValNone, it.Next())
	require.Equal(t, chunkenc.ValNone, it.Next())
	// Should also not be able to seek backwards again.
	require.Equal(t, chunkenc.ValNone, it.Seek(1))
}

func TestFromQueryResultWithDuplicates(t *testing.T) {
	ts1 := prompb.TimeSeries{
		Labels: []prompb.Label{
			{Name: "foo", Value: "bar"},
			{Name: "foo", Value: "def"},
		},
		Samples: []prompb.Sample{
			{Value: 0.0, Timestamp: 0},
		},
	}

	res := prompb.QueryResult{
		Timeseries: []*prompb.TimeSeries{
			&ts1,
		},
	}

	series := FromQueryResult(false, &res)

	errSeries, isErrSeriesSet := series.(errSeriesSet)

	require.True(t, isErrSeriesSet, "Expected resulting series to be an errSeriesSet")
	errMessage := errSeries.Err().Error()
	require.Equal(t, "duplicate label with name: foo", errMessage, fmt.Sprintf("Expected error to be from duplicate label, but got: %s", errMessage))
}

func TestNegotiateResponseType(t *testing.T) {
	r, err := NegotiateResponseType([]prompb.ReadRequest_ResponseType{
		prompb.ReadRequest_STREAMED_XOR_CHUNKS,
		prompb.ReadRequest_SAMPLES,
	})
	require.NoError(t, err)
	require.Equal(t, prompb.ReadRequest_STREAMED_XOR_CHUNKS, r)

	r2, err := NegotiateResponseType([]prompb.ReadRequest_ResponseType{
		prompb.ReadRequest_SAMPLES,
		prompb.ReadRequest_STREAMED_XOR_CHUNKS,
	})
	require.NoError(t, err)
	require.Equal(t, prompb.ReadRequest_SAMPLES, r2)

	r3, err := NegotiateResponseType([]prompb.ReadRequest_ResponseType{})
	require.NoError(t, err)
	require.Equal(t, prompb.ReadRequest_SAMPLES, r3)

	_, err = NegotiateResponseType([]prompb.ReadRequest_ResponseType{20})
	require.Error(t, err, "expected error due to not supported requested response types")
	require.Equal(t, "server does not support any of the requested response types: [20]; supported: map[SAMPLES:{} STREAMED_XOR_CHUNKS:{}]", err.Error())
}

func TestMergeLabels(t *testing.T) {
	for _, tc := range []struct {
		primary, secondary, expected []prompb.Label
	}{
		{
			primary:   []prompb.Label{{Name: "aaa", Value: "foo"}, {Name: "bbb", Value: "foo"}, {Name: "ddd", Value: "foo"}},
			secondary: []prompb.Label{{Name: "bbb", Value: "bar"}, {Name: "ccc", Value: "bar"}},
			expected:  []prompb.Label{{Name: "aaa", Value: "foo"}, {Name: "bbb", Value: "foo"}, {Name: "ccc", Value: "bar"}, {Name: "ddd", Value: "foo"}},
		},
		{
			primary:   []prompb.Label{{Name: "bbb", Value: "bar"}, {Name: "ccc", Value: "bar"}},
			secondary: []prompb.Label{{Name: "aaa", Value: "foo"}, {Name: "bbb", Value: "foo"}, {Name: "ddd", Value: "foo"}},
			expected:  []prompb.Label{{Name: "aaa", Value: "foo"}, {Name: "bbb", Value: "bar"}, {Name: "ccc", Value: "bar"}, {Name: "ddd", Value: "foo"}},
		},
	} {
		require.Equal(t, tc.expected, MergeLabels(tc.primary, tc.secondary))
	}
}

func TestDecodeWriteRequest(t *testing.T) {
	buf, _, _, err := buildWriteRequest(nil, writeRequestFixture.Timeseries, nil, nil, nil, nil, "snappy")
	require.NoError(t, err)

	actual, err := DecodeWriteRequest(bytes.NewReader(buf))
	require.NoError(t, err)
	require.Equal(t, writeRequestFixture, actual)
}

func TestDecodeV2WriteRequest(t *testing.T) {
	buf, _, _, err := buildV2WriteRequest(log.NewNopLogger(), writeV2RequestFixture.Timeseries, writeV2RequestFixture.Symbols, nil, nil, nil, "snappy")
	require.NoError(t, err)

	actual, err := DecodeV2WriteRequestStr(bytes.NewReader(buf))
	require.NoError(t, err)
	require.Equal(t, writeV2RequestFixture, actual)
}

func TestStreamResponse(t *testing.T) {
	lbs1 := prompb.FromLabels(labels.FromStrings("instance", "localhost1", "job", "demo1"), nil)
	lbs2 := prompb.FromLabels(labels.FromStrings("instance", "localhost2", "job", "demo2"), nil)
	chunk := prompb.Chunk{
		Type: prompb.Chunk_XOR,
		Data: make([]byte, 100),
	}
	lbSize, chunkSize := 0, chunk.Size()
	for _, lb := range lbs1 {
		lbSize += lb.Size()
	}
	maxBytesInFrame := lbSize + chunkSize*2
	testData := []*prompb.ChunkedSeries{{
		Labels: lbs1,
		Chunks: []prompb.Chunk{chunk, chunk, chunk, chunk},
	}, {
		Labels: lbs2,
		Chunks: []prompb.Chunk{chunk, chunk, chunk, chunk},
	}}
	css := newMockChunkSeriesSet(testData)
	writer := mockWriter{}
	warning, err := StreamChunkedReadResponses(&writer, 0,
		css,
		nil,
		maxBytesInFrame,
		&sync.Pool{})
	require.Nil(t, warning)
	require.NoError(t, err)
	expectData := []*prompb.ChunkedSeries{{
		Labels: lbs1,
		Chunks: []prompb.Chunk{chunk, chunk},
	}, {
		Labels: lbs1,
		Chunks: []prompb.Chunk{chunk, chunk},
	}, {
		Labels: lbs2,
		Chunks: []prompb.Chunk{chunk, chunk},
	}, {
		Labels: lbs2,
		Chunks: []prompb.Chunk{chunk, chunk},
	}}
	require.Equal(t, expectData, writer.actual)
}

type mockWriter struct {
	actual []*prompb.ChunkedSeries
}

func (m *mockWriter) Write(p []byte) (n int, err error) {
	cr := &prompb.ChunkedReadResponse{}
	if err := proto.Unmarshal(p, cr); err != nil {
		return 0, fmt.Errorf("unmarshaling: %w", err)
	}
	m.actual = append(m.actual, cr.ChunkedSeries...)
	return len(p), nil
}

type mockChunkSeriesSet struct {
	chunkedSeries []*prompb.ChunkedSeries
	index         int
	builder       labels.ScratchBuilder
}

func newMockChunkSeriesSet(ss []*prompb.ChunkedSeries) storage.ChunkSeriesSet {
	return &mockChunkSeriesSet{chunkedSeries: ss, index: -1, builder: labels.NewScratchBuilder(0)}
}

func (c *mockChunkSeriesSet) Next() bool {
	c.index++
	return c.index < len(c.chunkedSeries)
}

func (c *mockChunkSeriesSet) At() storage.ChunkSeries {
	return &storage.ChunkSeriesEntry{
		Lset: c.chunkedSeries[c.index].ToLabels(&c.builder, nil),
		ChunkIteratorFn: func(chunks.Iterator) chunks.Iterator {
			return &mockChunkIterator{
				chunks: c.chunkedSeries[c.index].Chunks,
				index:  -1,
			}
		},
	}
}

func (c *mockChunkSeriesSet) Warnings() annotations.Annotations { return nil }

func (c *mockChunkSeriesSet) Err() error {
	return nil
}

type mockChunkIterator struct {
	chunks []prompb.Chunk
	index  int
}

func (c *mockChunkIterator) At() chunks.Meta {
	one := c.chunks[c.index]
	chunk, err := chunkenc.FromData(chunkenc.Encoding(one.Type), one.Data)
	if err != nil {
		panic(err)
	}
	return chunks.Meta{
		Chunk:   chunk,
		MinTime: one.MinTimeMs,
		MaxTime: one.MaxTimeMs,
	}
}

func (c *mockChunkIterator) Next() bool {
	c.index++
	return c.index < len(c.chunks)
}

func (c *mockChunkIterator) Err() error {
	return nil
}

func v2RequesToWriteRequest(v2Req *writev2.Request) (*prompb.WriteRequest, error) {
	req := &prompb.WriteRequest{
		Timeseries: make([]prompb.TimeSeries, len(v2Req.Timeseries)),
		// TODO handle metadata?
	}
	b := labels.NewScratchBuilder(0)
	for i, rts := range v2Req.Timeseries {
		rts.ToLabels(&b, v2Req.Symbols).Range(func(l labels.Label) {
			req.Timeseries[i].Labels = append(req.Timeseries[i].Labels, prompb.Label{
				Name:  l.Name,
				Value: l.Value,
			})
		})

		exemplars := make([]prompb.Exemplar, len(rts.Exemplars))
		for j, e := range rts.Exemplars {
			exemplars[j].Value = e.Value
			exemplars[j].Timestamp = e.Timestamp
			e.ToExemplar(&b, v2Req.Symbols).Labels.Range(func(l labels.Label) {
				exemplars[j].Labels = append(exemplars[j].Labels, prompb.Label{
					Name:  l.Name,
					Value: l.Value,
				})
			})
		}
		req.Timeseries[i].Exemplars = exemplars

		req.Timeseries[i].Samples = make([]prompb.Sample, len(rts.Samples))
		for j, s := range rts.Samples {
			req.Timeseries[i].Samples[j].Timestamp = s.Timestamp
			req.Timeseries[i].Samples[j].Value = s.Value
		}

		req.Timeseries[i].Histograms = make([]prompb.Histogram, len(rts.Histograms))
		for j, h := range rts.Histograms {
			// TODO: double check
			if h.IsFloatHistogram() {
				req.Timeseries[i].Histograms[j].Count = &prompb.Histogram_CountFloat{CountFloat: h.GetCountFloat()}
				req.Timeseries[i].Histograms[j].ZeroCount = &prompb.Histogram_ZeroCountFloat{ZeroCountFloat: h.GetZeroCountFloat()}
			} else {
				req.Timeseries[i].Histograms[j].Count = &prompb.Histogram_CountInt{CountInt: h.GetCountInt()}
				req.Timeseries[i].Histograms[j].ZeroCount = &prompb.Histogram_ZeroCountInt{ZeroCountInt: h.GetZeroCountInt()}
			}

			for _, span := range h.NegativeSpans {
				req.Timeseries[i].Histograms[j].NegativeSpans = append(req.Timeseries[i].Histograms[j].NegativeSpans, prompb.BucketSpan{
					Offset: span.Offset,
					Length: span.Length,
				})
			}
			for _, span := range h.PositiveSpans {
				req.Timeseries[i].Histograms[j].PositiveSpans = append(req.Timeseries[i].Histograms[j].PositiveSpans, prompb.BucketSpan{
					Offset: span.Offset,
					Length: span.Length,
				})
			}

			req.Timeseries[i].Histograms[j].Sum = h.Sum
			req.Timeseries[i].Histograms[j].Schema = h.Schema
			req.Timeseries[i].Histograms[j].ZeroThreshold = h.ZeroThreshold
			req.Timeseries[i].Histograms[j].NegativeDeltas = h.NegativeDeltas
			req.Timeseries[i].Histograms[j].NegativeCounts = h.NegativeCounts
			req.Timeseries[i].Histograms[j].PositiveDeltas = h.PositiveDeltas
			req.Timeseries[i].Histograms[j].PositiveCounts = h.PositiveCounts
			req.Timeseries[i].Histograms[j].ResetHint = prompb.Histogram_ResetHint(h.ResetHint)
			req.Timeseries[i].Histograms[j].Timestamp = h.Timestamp
		}
	}
	return req, nil
}
