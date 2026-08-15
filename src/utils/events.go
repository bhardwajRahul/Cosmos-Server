package utils

import (
	"time"
	"encoding/json"
	"sync/atomic"
)

// errorEventFlushPending guards against spawning one flush goroutine per event
// during an error storm: only one prompt flush is in flight at a time; events
// buffered while it runs are picked up by the next error or the minute ticker.
var errorEventFlushPending atomic.Bool

func TriggerEvent(eventId string, label string, level string, object string, data map[string]interface{}) {
	// Skip debug events entirely when the configured log level is INFO or above
	if level == "debug" && LoggingLevelLabels[GetMainConfig().LoggingLevel] > DEBUG {
		return
	}

	Debug("Triggering event " + eventId)

	// Marshal the data map into a JSON string
	dataAsBytes, err := json.Marshal(data)
	if err != nil {
		Error("Error marshaling data: %v\n", err)
		return
	}
	dataAsString := string(dataAsBytes)

	BufferedDBWrite("events", map[string]interface{}{
		"eventId": eventId,
		"label": label,
		"application": "Cosmos",
		"level": level,
		"date": time.Now(),
		"data": data,
		"object": object,
		"_search": eventId + " " + dataAsString,
	})

	// Error-level events (quarantines, failures) shouldn't sit in the write
	// buffer for up to a minute before monitoring can see them — kick an async
	// flush so they land in the DB within seconds. flushBuffer takes bufferLock,
	// so running it from a goroutine is safe.
	if level == "error" && errorEventFlushPending.CompareAndSwap(false, true) {
		go func() {
			defer errorEventFlushPending.Store(false)
			flushBuffer("events")
		}()
	}
}
