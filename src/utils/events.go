package utils

import (
	"time"
)

func TriggerEvent(eventId string, label string, level string, object string, data map[string]interface{}) {
	// Skip debug events entirely when the configured log level is INFO or above
	if level == "debug" && LoggingLevelLabels[GetMainConfig().LoggingLevel] > DEBUG {
		return
	}

	Debug("Triggering event " + eventId)

	// direct insert: WAL makes a single row cheap, so there is no write buffer to drain
	err := InsertEvent(Event{
		EventId:     eventId,
		Label:       label,
		Application: "Cosmos",
		Level:       level,
		Date:        time.Now(),
		Data:        data,
		Object:      object,
	})

	// never route through Error(): MajorError triggers events, and that would recurse
	if err != nil {
		Debug("Error writing event " + eventId + ": " + err.Error())
	}
}
