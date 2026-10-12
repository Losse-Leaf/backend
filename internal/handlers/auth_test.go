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
	r.DELETE("/api/v1/auth/devices/:id", RevokeDevice)

	var ticketStr, pinCode string

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
				PinCode          string `json:"pin_code"`
				ExpiresInSeconds int    `json:"expires_in_seconds"`
				QRPayload        string `json:"qr_payload"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		if res.Data.Ticket == "" || len(res.Data.PinCode) != 6 {
			t.Fatalf("invalid ticket or pin code: %+v", res.Data)
		}

		ticketStr = res.Data.Ticket
		pinCode = res.Data.PinCode
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

	t.Run("4. Confirm pairing with PIN code on second ticket", func(t *testing.T) {
		// 先初始化第二个工单
		ticket2 := models.PairTicket{
			Ticket:    "pair_tk_test_pin_ticket",
			PinCode:   "888666",
			ExpiresAt: time.Now().Add(5 * time.Minute),
			Used:      false,
		}
		db.DB.Create(&ticket2)

		payload := ConfirmPairingRequest{
			PinCode:    "888666",
			DeviceName: "Test iPhone 16",
			Platform:   "ios",
		}
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/pair/confirm", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 for PIN confirm, got %d, body: %s", w.Code, w.Body.String())
		}
	})

	t.Run("4b. Brute-force PIN protection invalidates ticket after 5 failed attempts", func(t *testing.T) {
		ticket3 := models.PairTicket{
			Ticket:    "pair_tk_bruteforce_test",
			PinCode:   "999111",
			ExpiresAt: time.Now().Add(5 * time.Minute),
			Used:      false,
		}
		db.DB.Create(&ticket3)

		// 5 次错误 PIN 尝试
		for i := 0; i < 5; i++ {
			badPayload := ConfirmPairingRequest{
				PinCode: "000000",
			}
			body, _ := json.Marshal(badPayload)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/pair/confirm", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 for incorrect pin attempt %d, got %d", i+1, w.Code)
			}
		}

		// 检查数据库中该工单已被废弃
		var checkTk models.PairTicket
		db.DB.Where("ticket = ?", ticket3.Ticket).First(&checkTk)
		if !checkTk.Used || checkTk.FailedAttempts != 5 {
			t.Fatalf("expected ticket to be marked used with failed_attempts=5, got used=%v, failed_attempts=%d", checkTk.Used, checkTk.FailedAttempts)
		}

		// 第 6 次即便输入正确 PIN，也无法配对成功
		correctPayload := ConfirmPairingRequest{
			PinCode: "999111",
		}
		body, _ := json.Marshal(correctPayload)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/pair/confirm", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for invalidated ticket even with correct pin, got %d", w.Code)
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
	_ = pinCode
}
