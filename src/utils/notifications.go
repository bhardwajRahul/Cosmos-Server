package utils

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type NotificationActions struct {
	Text string
	Link string
}

type Notification struct {
	ID        int64
	Title     string
	Message   string
	Vars      string
	Icon      string
	Link      string
	Date      time.Time
	Level     string
	Read      bool
	Recipient string
	Actions   []NotificationActions
}

// NotifGet godoc
// @Summary Get notifications for the authenticated user
// @Tags Notifications
// @Produce json
// @Param from query string false "Pagination cursor (notification id for older notifications)"
// @Security BearerAuth
// @Success 200 {object} map[string]interface{}
// @Failure 403 {object} utils.HTTPErrorResult
// @Failure 500 {object} utils.HTTPErrorResult
// @Router /api/notifications [get]
func NotifGet(w http.ResponseWriter, req *http.Request) {
	from, _ := strconv.ParseInt(req.URL.Query().Get("from"), 10, 64)

	if CheckPermissions(w, req, PERM_LOGIN) != nil {
		return
	}

	nickname := GetAuthContext(req).Nickname

	if req.Method == "GET" {
		Debug("Notifications: Get notif for " + nickname)

		notifications, err := ListNotifications(nickname, from, 20)
		if err != nil {
			Error("Notifications: Error while getting notifications", err)
			HTTPError(w, "notifications Get Error", http.StatusInternalServerError, "UD001")
			return
		}

		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "OK",
			"data":   notifications,
		})
	} else {
		Error("Notifications: Method not allowed"+req.Method, nil)
		HTTPError(w, "Method not allowed", http.StatusMethodNotAllowed, "HTTP001")
		return
	}
}

// MarkAsRead godoc
// @Summary Mark notifications as read
// @Tags Notifications
// @Produce json
// @Param ids query string true "Comma-separated list of notification ids"
// @Security BearerAuth
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} utils.HTTPErrorResult
// @Failure 403 {object} utils.HTTPErrorResult
// @Failure 404 {object} utils.HTTPErrorResult
// @Failure 500 {object} utils.HTTPErrorResult
// @Router /api/notifications/read [get]
func MarkAsRead(w http.ResponseWriter, req *http.Request) {
	if req.Method == "GET" {
		if CheckPermissions(w, req, PERM_LOGIN) != nil {
			return
		}

		notificationIDs := []int64{}
		nickname := GetAuthContext(req).Nickname

		notificationIDsRaw := strings.Split(req.URL.Query().Get("ids"), ",")

		Debug(fmt.Sprintf("Marking %v notifications as read", notificationIDsRaw))

		for _, raw := range notificationIDsRaw {
			id, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				HTTPError(w, "Invalid notification ID "+raw, http.StatusBadRequest, "InvalidID")
				return
			}
			notificationIDs = append(notificationIDs, id)
		}

		matched, err := MarkNotificationsRead(nickname, notificationIDs)
		if err != nil {
			Error("Notifications: Error while marking notification as read", err)
			HTTPError(w, "Error updating notification", http.StatusInternalServerError, "UpdateError")
			return
		}

		if matched == 0 {
			HTTPError(w, "No matching notification found", http.StatusNotFound, "NotFound")
			return
		}

		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "OK",
			"message": "Notification marked as read",
		})
	} else {
		Error("Notifications: Method not allowed"+req.Method, nil)
		HTTPError(w, "Method not allowed", http.StatusMethodNotAllowed, "HTTP001")
		return
	}
}

func WriteNotification(notification Notification) {
	notification.Date = time.Now()
	notification.Read = false

	recipients := []string{notification.Recipient}

	// group recipients fan out to one row per user at write time
	if notification.Recipient == "all" || notification.Recipient == "admin" || notification.Recipient == "user" {
		users := ListAllUsers(notification.Recipient)
		Debug("Notifications: Sending notification to " + fmt.Sprint(len(users)) + " users")

		recipients = []string{}
		for _, user := range users {
			recipients = append(recipients, user.Nickname)
		}
	}

	for _, recipient := range recipients {
		notification.Recipient = recipient
		if err := InsertNotification(notification); err != nil {
			Error("Notifications: Error while writing notification", err)
			return
		}
	}
}
