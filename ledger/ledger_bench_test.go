package ledger

import (
	"sort"
	"strconv"
	"testing"
	"time"
)

// BenchmarkPost measures single-goroutine Post throughput and the per-op
// latency distribution. Latency is sampled around every Post so a p99 can
// be reported; the resulting numbers feed the benchmark table in the README.
// Run with: go test -run=NONE -bench=BenchmarkPost -benchtime=3s ./ledger/
func BenchmarkPost(b *testing.B) {
	l := New()
	lat := make([]time.Duration, 0, b.N)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e := JournalEntry{
			ID:            "bench-" + strconv.Itoa(i),
			DebitAccount:  "cash",
			CreditAccount: "equity",
			AmountCents:   100,
		}
		start := time.Now()
		if _, _, err := l.Post(e); err != nil {
			b.Fatalf("Post: %v", err)
		}
		lat = append(lat, time.Since(start))
	}
	b.StopTimer()

	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	p50 := lat[len(lat)/2]
	p99 := lat[int(0.99*float64(len(lat)))]

	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "entries/sec")
	b.ReportMetric(float64(p50.Microseconds()), "p50-us/op")
	b.ReportMetric(float64(p99.Microseconds()), "p99-us/op")
}
