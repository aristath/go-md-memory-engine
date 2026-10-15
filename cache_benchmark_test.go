package memory

import (
	"fmt"
	"math"
	"testing"
)

// This isolates codec cost, not retrieval quality: generated dense normalized
// vectors model 10,000 chunks with the live provider's 768-dimensional shape.
func BenchmarkVectorCacheDecode(b *testing.B) {
	values := make([]float64, 768)
	norm := 0.0
	for i := range values {
		values[i] = math.Sin(float64(i + 1))
		norm += values[i] * values[i]
	}
	for i := range values {
		values[i] /= math.Sqrt(norm)
	}
	data := make(map[string]vector, 10000)
	for i := 0; i < 10000; i++ {
		data[fmt.Sprint(i)] = vector{Content: "content", Config: "config", Values: [][]float64{values}}
	}
	encoded, e := encodeVectors(data)
	if e != nil {
		b.Fatal(e)
	}
	b.SetBytes(int64(len(encoded)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var decoded map[string]vector
		if e := decodeVectors(encoded, &decoded); e != nil {
			b.Fatal(e)
		}
		if len(decoded) != 10000 {
			b.Fatal("incomplete decode")
		}
	}
	b.ReportMetric(float64(len(encoded)), "cache-bytes")
}
