package metrics

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/azukaar/cosmos-server/src/utils"
)

type Event = utils.Event

// levelLadder maps the requested minimum level to the levels it includes.
var levelLadder = map[string][]string{
	"debug":     {"debug", "info", "warning", "error", "important", "success"},
	"info":      {"info", "warning", "error", "important", "success"},
	"success":   {"warning", "error", "important", "success"},
	"warning":   {"warning", "error", "important"},
	"important": {"important", "error"},
}

// API_ListEvents godoc
// @Summary List events with filtering and pagination
// @Tags Metrics
// @Produce json
// @Param from query string false "Start date in RFC3339 format (2006-01-02T15:04:05Z)"
// @Param to query string false "End date in RFC3339 format (2006-01-02T15:04:05Z)"
// @Param logLevel query string false "Minimum log level (debug, info, success, warning, important, error)" default(info)
// @Param search query string false "Full-text search term"
// @Param query query string false "Filter as a JSON object"
// @Param page query string false "Pagination cursor (event id)"
// @Security BearerAuth
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} utils.HTTPErrorResult
// @Failure 403 {object} utils.HTTPErrorResult
// @Failure 500 {object} utils.HTTPErrorResult
// @Router /api/events [get]
func API_ListEvents(w http.ResponseWriter, req *http.Request) {
	if utils.CheckPermissions(w, req, utils.PERM_RESOURCES_READ) != nil {
		return
	}

	if req.Method != "GET" {
		utils.Error("events: Method not allowed"+req.Method, nil)
		utils.HTTPError(w, "Method not allowed", http.StatusMethodNotAllowed, "HTTP001")
		return
	}

	query := req.URL.Query()

	from, errF := time.Parse("2006-01-02T15:04:05Z", query.Get("from"))
	if errF != nil {
		utils.Error("events: Error while parsing from date", errF)
	}
	to, errF := time.Parse("2006-01-02T15:04:05Z", query.Get("to"))
	if errF != nil {
		utils.Error("events: Error while parsing to date", errF)
	}

	logLevel := query.Get("logLevel")
	if logLevel == "" {
		logLevel = "info"
	}
	levels, ok := levelLadder[logLevel]
	if !ok {
		levels = []string{"error"}
	}

	// "" and "0" both mean "first page" (older clients sent page=0)
	page, _ := strconv.ParseInt(query.Get("page"), 10, 64)

	eventQuery := utils.EventQuery{
		From:   from,
		To:     to,
		Levels: levels,
		Search: query.Get("search"),
		Cursor: page,
		Limit:  50,
	}

	if raw := query.Get("query"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &eventQuery.Filter); err != nil {
			utils.Error("events: Error while parsing query "+raw, err)
			utils.HTTPError(w, "Invalid query: "+err.Error(), http.StatusBadRequest, "UD001")
			return
		}
	}

	events, totalCount, err := utils.QueryEvents(eventQuery)
	if err != nil {
		utils.Error("events: Error while getting events", err)
		// a rejected filter is the user's typo, not a server fault
		if len(eventQuery.Filter) > 0 {
			utils.HTTPError(w, "Unsupported query: "+err.Error(), http.StatusBadRequest, "UD001")
			return
		}
		utils.HTTPError(w, "events Get Error", http.StatusInternalServerError, "UD001")
		return
	}

	w.Header().Set("Content-Type", "application/json")

	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "OK",
		"total":  totalCount,
		"data":   events,
	})
}
