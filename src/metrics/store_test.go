package metrics

import (
	"testing"
	"time"

	"github.com/azukaar/cosmos-server/src/utils"
)

func setupMetricsStore(t *testing.T) {
	t.Helper()
	tmp := t.TempDir() + "/"
	prev := utils.CONFIGFOLDER
	utils.CONFIGFOLDER = tmp
	if err := utils.InitMetricsDatabase(); err != nil {
		t.Fatal("InitMetricsDatabase:", err)
	}

	lock <- true
	dataBuffer = map[string]DataPush{}
	lastInserted = map[string]int{}
	lastHourPool = time.Time{}
	lastDayPool = time.Time{}
	<-lock

	t.Cleanup(func() {
		utils.CloseMetricsDatabase()
		utils.CONFIGFOLDER = prev
	})
}

func loadOne(t *testing.T, key string) DataDefDB {
	t.Helper()
	docs, err := loadMetrics([]string{key})
	if err != nil {
		t.Fatal("loadMetrics:", err)
	}
	if len(docs) != 1 {
		t.Fatalf("expected 1 metric for %q, got %d", key, len(docs))
	}
	return docs[0]
}

// Re-flushing an open bucket must correct its own contribution rather than replay it.
// Every SetOperation is exercised across two flushes of the same bucket.
func TestFlushSetOperationsAcrossTwoFlushes(t *testing.T) {
	cases := []struct {
		name      string
		operation string
		aggloType string
		first     []int
		second    []int
		wantRaw   int
		wantHour  int
	}{
		{"replace", "", "", []int{3, 4}, []int{5}, 5, 5},
		{"sum", "sum", "sum", []int{3, 4}, []int{5}, 12, 12},
		{"max", "max", "sum", []int{3, 9}, []int{5}, 9, 9},
		{"min", "min", "sum", []int{9, 3}, []int{5}, 3, 3},
		{"avg", "avg", "avg", []int{10, 20}, []int{30}, 20, 20},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setupMetricsStore(t)

			def := DataDef{
				// an hour-long bucket cannot expire mid-test
				Period:       time.Hour,
				Label:        "test " + c.name,
				SetOperation: c.operation,
				AggloType:    c.aggloType,
			}
			key := "test." + c.name

			for _, v := range c.first {
				pushSetMetric(key, v, def)
			}
			SaveMetrics()
			for _, v := range c.second {
				pushSetMetric(key, v, def)
			}
			SaveMetrics()

			doc := loadOne(t, "cosmos."+key)

			if len(doc.Values) != 1 {
				t.Fatalf("expected one raw bucket, got %d", len(doc.Values))
			}
			if doc.Values[0].Value != c.wantRaw {
				t.Fatalf("raw value = %d, want %d", doc.Values[0].Value, c.wantRaw)
			}

			hourKey := AggloKey(granularityHour, ModuloTime(doc.Values[0].Date, time.Hour))
			hour, ok := doc.ValuesAggl[hourKey]
			if !ok {
				t.Fatalf("hour pool %q missing from %v", hourKey, doc.ValuesAggl)
			}
			if hour.Value != c.wantHour {
				t.Fatalf("hour pool = %d, want %d", hour.Value, c.wantHour)
			}

			dayKey := AggloKey(granularityDay, ModuloTime(doc.Values[0].Date, 24*time.Hour))
			if day, ok := doc.ValuesAggl[dayKey]; !ok || day.Value != c.wantHour {
				t.Fatalf("day pool %q = %+v, want %d", dayKey, day, c.wantHour)
			}
		})
	}
}

// A bucket that gained no new samples must not be re-contributed to its pools.
func TestFlushWithoutNewSamplesIsIdempotent(t *testing.T) {
	setupMetricsStore(t)

	def := DataDef{Period: time.Hour, SetOperation: "sum", AggloType: "sum", Label: "idem"}
	pushSetMetric("test.idem", 7, def)

	SaveMetrics()
	SaveMetrics()
	SaveMetrics()

	doc := loadOne(t, "cosmos.test.idem")
	hourKey := AggloKey(granularityHour, ModuloTime(doc.Values[0].Date, time.Hour))
	if doc.Values[0].Value != 7 || doc.ValuesAggl[hourKey].Value != 7 {
		t.Fatalf("repeat flushes double counted: raw %d, hour %d",
			doc.Values[0].Value, doc.ValuesAggl[hourKey].Value)
	}
}

func TestAggloKeyFormatting(t *testing.T) {
	date := time.Date(2024, 5, 1, 13, 45, 30, 0, time.UTC)

	if got := AggloKey(granularityHour, ModuloTime(date, time.Hour)); got != "hour_2024-05-01 13:00:00" {
		t.Fatalf("hour key = %q", got)
	}
	if got := AggloKey(granularityDay, ModuloTime(date, 24*time.Hour)); got != "day_2024-05-01" {
		t.Fatalf("day key = %q", got)
	}
}

// The daily alert lookup used to format its pool key differently from the writer, so
// it never matched. Both periods now round with ModuloTime and are read back by it.
func TestPoolAlertLookupMatchesWrittenBuckets(t *testing.T) {
	setupMetricsStore(t)

	date := time.Date(2024, 5, 1, 13, 45, 0, 0, time.UTC)
	err := writeBuckets([]DataPush{{
		Date:      date,
		Key:       "cosmos.test.pools",
		Value:     42,
		Period:    30 * time.Second,
		AggloType: "sum",
		Label:     "pools",
		Object:    "container@x",
		Max:       100,
	}})
	if err != nil {
		t.Fatal("writeBuckets:", err)
	}

	for _, pool := range []struct {
		granularity string
		period      time.Duration
	}{
		{granularityHour, time.Hour},
		{granularityDay, 24 * time.Hour},
	} {
		pools, err := poolValues(pool.granularity, ModuloTime(date, pool.period))
		if err != nil {
			t.Fatal("poolValues:", err)
		}
		row, ok := pools["cosmos.test.pools"]
		if !ok {
			t.Fatalf("%s pool not found at its own bucket date", pool.granularity)
		}
		if row.Value != 42 || row.Object != "container@x" || row.Max != 100 {
			t.Fatalf("%s pool row = %+v", pool.granularity, row)
		}
	}
}

func TestLoadMetricsWildcardAndCatalog(t *testing.T) {
	setupMetricsStore(t)

	def := DataDef{Period: time.Hour, AggloType: "avg", Label: "CPU", Unit: "%", Max: 100, Scale: 3}
	pushSetMetric("system.cpu.0", 40, def)
	pushSetMetric("system.cpu.1", 60, def)
	pushSetMetric("system.ram", 10, DataDef{Period: time.Hour, Label: "RAM"})
	SaveMetrics()

	docs, err := loadMetrics([]string{"cosmos.system.cpu.*"})
	if err != nil || len(docs) != 2 {
		t.Fatalf("wildcard match = %d docs, %v", len(docs), err)
	}
	if docs[0].Label != "CPU" || docs[0].Unit != "%" || docs[0].Max != 100 || docs[0].Scale != 3 {
		t.Fatalf("catalog fields lost: %+v", docs[0])
	}
	// Period / 30s
	if docs[0].TimeScale != 120 {
		t.Fatalf("TimeScale = %v, want 120", docs[0].TimeScale)
	}

	exact, err := loadMetrics([]string{"cosmos.system.ram"})
	if err != nil || len(exact) != 1 || exact[0].Label != "RAM" {
		t.Fatalf("exact match = %+v, %v", exact, err)
	}

	all, err := loadMetrics([]string{})
	if err != nil || len(all) != 3 {
		t.Fatalf("empty list should return everything, got %d, %v", len(all), err)
	}
}

func TestResetLocalMetrics(t *testing.T) {
	setupMetricsStore(t)

	pushSetMetric("test.reset", 1, DataDef{Period: time.Hour, Label: "reset"})
	SaveMetrics()

	if docs, _ := loadMetrics([]string{}); len(docs) != 1 {
		t.Fatal("setup did not persist a metric")
	}
	if err := resetLocalMetrics(); err != nil {
		t.Fatal("resetLocalMetrics:", err)
	}
	if docs, _ := loadMetrics([]string{}); len(docs) != 0 {
		t.Fatalf("reset left %d metrics behind", len(docs))
	}
}
