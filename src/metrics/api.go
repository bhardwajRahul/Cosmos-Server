package metrics

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/azukaar/cosmos-server/src/utils"
)

// AggloMetrics returns the dashboard documents for the requested keys.
// Agglomeration happens on write, so this is a plain read.
func AggloMetrics(metricsList []string) []DataDefDB {
	metrics, err := loadMetrics(metricsList)
	if err != nil {
		utils.Error("Metrics: Error fetching metrics", err)
		return []DataDefDB{}
	}
	return metrics
}

// API_GetMetrics godoc
// @Summary Get aggregated metrics data
// @Tags Metrics
// @Produce json
// @Param metrics query string false "Comma-separated list of metric keys to retrieve"
// @Security BearerAuth
// @Success 200 {object} map[string]interface{}
// @Failure 403 {object} utils.HTTPErrorResult
// @Router /api/metrics [get]
func API_GetMetrics(w http.ResponseWriter, req *http.Request) {
	if utils.CheckPermissions(w, req, utils.PERM_RESOURCES_READ) != nil {
		return
	}

	//get query string "metrics"
	query := req.URL.Query()
	metrics := query.Get("metrics")

	// split by comma
	metricsList := []string{}
	if metrics != "" {
		metricsList = strings.Split(metrics, ",")
	}

	if req.Method == "GET" {
		w.Header().Set("Content-Type", "application/json")

		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "OK",
			"data":   AggloMetrics(metricsList),
		})
	} else {
		utils.Error("MetricsGet: Method not allowed"+req.Method, nil)
		utils.HTTPError(w, "Method not allowed", http.StatusMethodNotAllowed, "HTTP001")
		return
	}
}

// API_ResetMetrics godoc
// @Summary Reset all metrics and events data
// @Tags Metrics
// @Produce json
// @Security BearerAuth
// @Success 200 {object} map[string]interface{}
// @Failure 403 {object} utils.HTTPErrorResult
// @Failure 500 {object} utils.HTTPErrorResult
// @Router /api/reset-metrics [get]
func API_ResetMetrics(w http.ResponseWriter, req *http.Request) {
	if utils.CheckPermissions(w, req, utils.PERM_RESOURCES) != nil {
		return
	}

	lock <- true
	dataBuffer = map[string]DataPush{}
	lastInserted = map[string]int{}
	<-lock

	if err := resetLocalMetrics(); err != nil {
		utils.Error("MetricsReset: Database error", err)
		utils.HTTPError(w, "Database error ", http.StatusInternalServerError, "DB001")
		return
	}

	if err := utils.DeleteLocalEvents(); err != nil {
		utils.Error("MetricsReset: Database error", err)
		utils.HTTPError(w, "Database error ", http.StatusInternalServerError, "DB001")
		return
	}

	if req.Method == "GET" {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "OK",
		})
	} else {
		utils.Error("SettingGet: Method not allowed"+req.Method, nil)
		utils.HTTPError(w, "Method not allowed", http.StatusMethodNotAllowed, "HTTP001")
		return
	}
}

type MetricList struct {
	Key   string
	Label string
}

// ListMetrics godoc
// @Summary List all available metric keys and their labels
// @Tags Metrics
// @Produce json
// @Security BearerAuth
// @Success 200 {object} map[string]interface{}
// @Failure 403 {object} utils.HTTPErrorResult
// @Failure 500 {object} utils.HTTPErrorResult
// @Router /api/list-metrics [get]
func ListMetrics(w http.ResponseWriter, req *http.Request) {
	if utils.CheckPermissions(w, req, utils.PERM_RESOURCES_READ) != nil {
		return
	}

	if req.Method == "GET" {
		metricNames, err := listMetricNames()
		if err != nil {
			utils.Error("metrics: Error while getting metrics", err)
			utils.HTTPError(w, "metrics Get Error", http.StatusInternalServerError, "UD001")
			return
		}

		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "OK",
			"data":   metricNames,
		})
	} else {
		utils.Error("metrics: Method not allowed"+req.Method, nil)
		utils.HTTPError(w, "Method not allowed", http.StatusMethodNotAllowed, "HTTP001")
		return
	}
}
