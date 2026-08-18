package metrics

import (
	"strconv"
	"time"

	"github.com/azukaar/cosmos-server/src/utils"
)

type DataDef struct {
	Max           uint64
	Period        time.Duration
	Label         string
	AggloType     string
	SetOperation  string
	Scale         int
	Unit          string
	Decumulate    bool
	DecumulatePos bool
	Object        string
}

type DataPush struct {
	Date          time.Time
	Key           string
	Value         int
	Max           uint64
	Period        time.Duration
	Expire        time.Time
	Label         string
	AvgIndex      int
	AggloType     string
	SetOperation  string
	Scale         int
	Unit          string
	Decumulate    bool
	DecumulatePos bool
	Object        string
	// Written / Contributed track what the last successful flush pushed, so re-flushing
	// an open bucket corrects its aggregation contribution instead of double counting.
	Written     int
	Contributed bool
}

type DataDefDBEntry struct {
	Date      time.Time
	Value     int
	Processed bool
	// For agglomeration
	AvgIndex    int
	AggloTo     time.Time
	AggloExpire time.Time
}

type DataDefDB struct {
	Values     []DataDefDBEntry
	ValuesAggl map[string]DataDefDBEntry
	LastUpdate time.Time
	TimeScale  float64
	Max        uint64
	Label      string
	Key        string
	AggloType  string
	Scale      int
	Unit       string
	Object     string
}

var dataBuffer = map[string]DataPush{}

var lock = make(chan bool, 1)

func GetDataBuffer() map[string]DataPush {
	return dataBuffer
}

func MergeMetric(SetOperation string, currentValue int, newValue int, avgIndex int) int {
	if SetOperation == "" {
		return newValue
	} else if SetOperation == "max" {
		if newValue > currentValue {
			return newValue
		} else {
			return currentValue
		}
	} else if SetOperation == "sum" {
		return currentValue + newValue
	} else if SetOperation == "min" {
		if newValue < currentValue {
			return newValue
		} else {
			return currentValue
		}
	} else if SetOperation == "avg" {
		if avgIndex == 0 {
			return newValue
		}
		return (currentValue*avgIndex + newValue) / (avgIndex + 1)
	} else {
		return newValue
	}
}

// SaveMetrics flushes every open bucket on each tick; only the delta since the
// last flush reaches the aggregation pools.
func SaveMetrics() {
	utils.Debug("Metrics - Saving data")

	lock <- true
	defer func() { <-lock }()

	now := time.Now()

	flushKeys := []string{}
	buckets := []DataPush{}
	for cacheKey, dp := range dataBuffer {
		if dp.Contributed && dp.Value == dp.Written {
			continue
		}
		flushKeys = append(flushKeys, cacheKey)
		buckets = append(buckets, dp)
	}

	if len(buckets) > 0 {
		if err := writeBuckets(buckets); err != nil {
			utils.Error("Metrics - Save Error", err)
		} else {
			for i, cacheKey := range flushKeys {
				dp := dataBuffer[cacheKey]
				dp.Written = buckets[i].Value
				dp.Contributed = true
				dataBuffer[cacheKey] = dp
			}
			utils.Debug("Data - Saved " + strconv.Itoa(len(buckets)) + " buckets")
		}
	} else {
		utils.Debug("No data to save")
	}

	// latest-period alerts fire once, when the bucket closes
	for cacheKey, dp := range dataBuffer {
		if dp.Expire.After(now) {
			continue
		}
		CheckAlerts(dp.Key, "latest", utils.AlertMetricTrack{
			Key:    dp.Key,
			Object: dp.Object,
			Max:    dp.Max,
		}, dp.Value)
		delete(dataBuffer, cacheKey)
	}

	checkPoolAlerts(now)
}

var lastHourPool time.Time
var lastDayPool time.Time

// checkPoolAlerts fires hourly/daily alerts on the pool that just closed.
func checkPoolAlerts(now time.Time) {
	hour := ModuloTime(now, time.Hour)
	day := ModuloTime(now, 24*time.Hour)

	if !lastHourPool.IsZero() && !hour.Equal(lastHourPool) {
		runPoolAlerts(granularityHour, "hourly", lastHourPool)
	}
	if !lastDayPool.IsZero() && !day.Equal(lastDayPool) {
		runPoolAlerts(granularityDay, "daily", lastDayPool)
	}

	lastHourPool = hour
	lastDayPool = day
}

func runPoolAlerts(granularity string, period string, poolDate time.Time) {
	pools, err := poolValues(granularity, poolDate)
	if err != nil {
		utils.Error("Metrics - Pool alerts", err)
		return
	}

	for _, pool := range pools {
		CheckAlerts(pool.Key, period, utils.AlertMetricTrack{
			Key:    pool.Key,
			Object: pool.Object,
			Max:    pool.Max,
		}, pool.Value)
	}
}

func ModuloTime(start time.Time, modulo time.Duration) time.Time {
	elapsed := start.UnixNano() // This gives us the total nanoseconds since 1970
	durationNano := modulo.Nanoseconds()

	// Here we take modulo of elapsed time with the duration to get the remainder.
	// Then, we subtract the remainder from the elapsed time to get the start of the period.
	roundedElapsed := elapsed - elapsed%durationNano

	// Convert back to time.Time
	return time.Unix(0, roundedElapsed)
}

var lastInserted = map[string]int{}

func PushSetMetric(key string, value int, def DataDef) {
	go pushSetMetric(key, value, def)
}

func pushSetMetric(key string, value int, def DataDef) {
	originalValue := value
	key = "cosmos." + key
	date := ModuloTime(time.Now(), def.Period)
	cacheKey := key + date.String()

	lock <- true
	defer func() { <-lock }()

	if def.Decumulate || def.DecumulatePos {
		if lastInserted[key] != 0 {
			value = value - lastInserted[key]
			if def.DecumulatePos && value < 0 {
				value = 0
			}
		} else {
			value = 0
		}
	}

	if dp, ok := dataBuffer[cacheKey]; ok {
		value = MergeMetric(def.SetOperation, dp.Value, value, dp.AvgIndex)

		dp.Max = def.Max
		dp.Value = value
		if def.SetOperation == "avg" {
			dp.AvgIndex++
		}

		dataBuffer[cacheKey] = dp
	} else {
		// the first sample already counts, or the running average would discard it
		avgIndex := 0
		if def.SetOperation == "avg" {
			avgIndex = 1
		}

		dataBuffer[cacheKey] = DataPush{
			Date:         date,
			Expire:       ModuloTime(time.Now().Add(def.Period), def.Period),
			Key:          key,
			Value:        value,
			AvgIndex:     avgIndex,
			Max:          def.Max,
			Label:        def.Label,
			AggloType:    def.AggloType,
			SetOperation: def.SetOperation,
			Scale:        def.Scale,
			Unit:         def.Unit,
			Object:       def.Object,
			Period:       def.Period,
		}
	}

	lastInserted[key] = originalValue
}

func Run() {
	nextTime := ModuloTime(time.Now().Add(time.Second*30), time.Second*30)
	nextTime = nextTime.Add(time.Second * 2)

	if utils.GetMainConfig().MonitoringDisabled {
		time.AfterFunc(nextTime.Sub(time.Now()), func() {
			Run()
		})

		return
	}

	utils.Debug("Metrics - Run - Next run at " + nextTime.String())

	time.AfterFunc(nextTime.Sub(time.Now()), func() {
		go func() {
			GetSystemMetrics()
			SaveMetrics()
		}()

		Run()
	})
}

func Init() {
	lastInserted = map[string]int{}

	Run()

	if !utils.GetMainConfig().MonitoringDisabled {
		go GetSystemMetrics()
	}
}
