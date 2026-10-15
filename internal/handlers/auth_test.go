package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"loose-leaf/internal/db"
	"loose-leaf/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func setupTestDB(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open test in-memory db: %v", err)
	}
	_ = database.AutoMigrate(&models.Device{}, &models.PairTicket{}, &models.SiteConfig{})
	database.Create(&models.SiteConfig{Initialized: true})
	db.DB = database
}

func TestDevicePairingLifecycle(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestDB(t)

	r := gin.New()
	r.POST("/api/v1/auth/pair/init", InitPairing)
	r.POST("/api/v1/auth/pair/confirm", ConfirmPairing)
	r.GET("/api/v1/auth/devices", GetTrustedDevices)
	r.PUT("/api/v1/auth/devices/:id", UpdateDeviceName)
	r.DELETE("/api/v1/auth/devices/:id", RevokeDevice)

	var ticketStr string

	t.Run("1. Init pairing ticket", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/pair/init", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}

		var res struct {
			Code int `json:"code"`
			Data struct {
				Ticket           string `json:"ticket"`
				ExpiresInSeconds int    `json:"expires_in_seconds"`
				QRPayload        string `json:"qr_payload"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		if res.Data.Ticket == "" || res.Data.QRPayload == "" {
			t.Fatalf("invalid ticket or qr_payload: %+v", res.Data)
		}

		ticketStr = res.Data.Ticket
	})

	var deviceToken, deviceID string

	t.Run("2. Confirm pairing with ticket", func(t *testing.T) {
		payload := ConfirmPairingRequest{
			Ticket:      ticketStr,
			DeviceName:  "Test MacBook Pro",
			Platform:    "macos",
			DeviceModel: "Mac15,3",
		}
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/pair/confirm", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
		}

		var res struct {
			Code int `json:"code"`
			Data struct {
				DeviceID    string `json:"device_id"`
				DeviceName  string `json:"device_name"`
				DeviceToken string `json:"device_token"`
			} `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &res)

		if res.Data.DeviceID == "" || res.Data.DeviceToken == "" {
			t.Fatalf("expected device_id and device_token, got: %+v", res.Data)
		}

		deviceID = res.Data.DeviceID
		deviceToken = res.Data.DeviceToken
	})

	t.Run("3. Reusing ticket is rejected", func(t *testing.T) {
		payload := ConfirmPairingRequest{
			Ticket: ticketStr,
		}
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/pair/confirm", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for already used ticket, got %d", w.Code)
		}
	})

	t.Run("4. Confirm pairing with second ticket", func(t *testing.T) {
		ticket2 := models.PairTicket{
			Ticket:    "pair_tk_test_second_ticket",
			ExpiresAt: time.Now().Add(5 * time.Minute),
			Used:      false,
		}
		db.DB.Create(&ticket2)

		payload := ConfirmPairingRequest{
			Ticket:     "pair_tk_test_second_ticket",
			DeviceName: "Test iPhone 16",
			Platform:   "ios",
		}
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/pair/confirm", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 for second ticket confirm, got %d, body: %s", w.Code, w.Body.String())
		}
	})

	t.Run("4b. Empty or invalid ticket is rejected without affecting valid tickets", func(t *testing.T) {
		ticket3 := models.PairTicket{
			Ticket:    "pair_tk_valid_ticket",
			ExpiresAt: time.Now().Add(5 * time.Minute),
			Used:      false,
		}
		db.DB.Create(&ticket3)

		badPayload := ConfirmPairingRequest{Ticket: ""}
		body, _ := json.Marshal(badPayload)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/pair/confirm", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for empty ticket, got %d", w.Code)
		}

		var checkTk models.PairTicket
		db.DB.Where("ticket = ?", ticket3.Ticket).First(&checkTk)
		if checkTk.Used {
			t.Fatalf("valid ticket must remain unused, got used=%v", checkTk.Used)
		}
	})

	t.Run("5. List trusted devices", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/devices", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}

		var res struct {
			Code int             `json:"code"`
			Data []models.Device `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &res)

		if len(res.Data) != 2 {
			t.Fatalf("expected 2 devices, got %d", len(res.Data))
		}
	})

	t.Run("5b. Rename active device via UpdateDeviceName", func(t *testing.T) {
		renameBody, _ := json.Marshal(map[string]string{"name": "  Studio MacBook Pro M3  "})
		req := httptest.NewRequest(http.MethodPut, "/api/v1/auth/devices/"+deviceID, bytes.NewReader(renameBody))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on rename, got %d: %s", w.Code, w.Body.String())
		}

		var res struct {
			Code int           `json:"code"`
			Data models.Device `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &res)
		if res.Data.Name != "Studio MacBook Pro M3" {
			t.Fatalf("expected trimmed name 'Studio MacBook Pro M3', got '%s'", res.Data.Name)
		}

		// Empty name should return 400
		emptyBody, _ := json.Marshal(map[string]string{"name": "   "})
		badReq := httptest.NewRequest(http.MethodPut, "/api/v1/auth/devices/"+deviceID, bytes.NewReader(emptyBody))
		badReq.Header.Set("Content-Type", "application/json")
		badW := httptest.NewRecorder()
		r.ServeHTTP(badW, badReq)
		if badW.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for empty device name, got %d", badW.Code)
		}
	})

	t.Run("6. Revoke device and verify it is excluded from GetTrustedDevices", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/auth/devices/"+deviceID, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}

		var device models.Device
		db.DB.Where("device_id = ?", deviceID).First(&device)
		if device.Status != "revoked" {
			t.Fatalf("expected status 'revoked', got '%s'", device.Status)
		}

		// Verify GetTrustedDevices only returns active devices
		listReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/devices", nil)
		listW := httptest.NewRecorder()
		r.ServeHTTP(listW, listReq)

		var res struct {
			Code int             `json:"code"`
			Data []models.Device `json:"data"`
		}
		_ = json.Unmarshal(listW.Body.Bytes(), &res)
		if len(res.Data) != 1 {
			t.Fatalf("expected 1 active device after revocation, got %d", len(res.Data))
		}
		if res.Data[0].DeviceID == deviceID {
			t.Fatalf("expected revoked device %s to be excluded from trusted devices list", deviceID)
		}
	})

	_ = deviceToken
}
